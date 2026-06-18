package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/agent"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/platform"
)

// botReplyTimeout bounds one platform-driven engine turn, so a stuck model can't
// pin a bot's message handler forever.
const botReplyTimeout = 3 * time.Minute

// botManager owns the running platform bots. Bots are long-lived outbound
// connections (e.g. DingTalk Stream), not request-scoped, so they live here and
// are restarted whenever config reloads — mirroring how Watch and the MCP
// toolset cache are managed.
type botManager struct {
	mu   sync.Mutex
	ctx  context.Context // server lifecycle ctx, set by StartBots
	bots []platform.Bot
}

// StartBots launches the configured platform bots and keeps them running until
// ctx is cancelled. Called once from each server entrypoint, alongside Watch.
func (s *Server) StartBots(ctx context.Context) {
	s.bots.mu.Lock()
	s.bots.ctx = ctx
	s.bots.mu.Unlock()

	s.restartBots(s.engine().Config())

	go func() {
		<-ctx.Done()
		s.stopBots()
	}()
}

// restartBots stops any running bots and starts the enabled ones from cfg. Safe
// to call on every config reload; a no-op until StartBots has supplied a ctx.
func (s *Server) restartBots(cfg *config.Config) {
	s.bots.mu.Lock()
	ctx := s.bots.ctx
	old := s.bots.bots
	s.bots.bots = nil
	s.bots.mu.Unlock()

	for _, b := range old {
		b.Stop()
	}
	if ctx == nil {
		return // StartBots not called yet (e.g. API-only tests)
	}

	var started []platform.Bot
	for _, pb := range cfg.Platforms {
		if !pb.Enabled {
			continue
		}
		bot, err := s.buildBot(pb)
		if err != nil {
			s.logf("平台 %q 跳过: %v", pb.Name, err)
			continue
		}
		started = append(started, bot)
		go func(b platform.Bot, name string) {
			if err := b.Start(ctx); err != nil {
				s.logf("平台 %q 启动失败: %v", name, err)
			}
		}(bot, pb.Name)
	}

	s.bots.mu.Lock()
	s.bots.bots = started
	s.bots.mu.Unlock()
}

// stopBots disconnects all running bots.
func (s *Server) stopBots() {
	s.bots.mu.Lock()
	old := s.bots.bots
	s.bots.bots = nil
	s.bots.mu.Unlock()
	for _, b := range old {
		b.Stop()
	}
}

// buildBot constructs one bot from its config, wiring a ReplyFunc that runs an
// engine turn keyed by the conversation.
func (s *Server) buildBot(pb config.PlatformBot) (platform.Bot, error) {
	switch pb.Type {
	case "dingtalk":
		if pb.ClientID == "" || pb.ClientSecret == "" {
			return nil, fmt.Errorf("dingtalk 需要 client_id 与 client_secret")
		}
		provider := pb.Provider
		reply := func(ctx context.Context, sessionKey, text string) (string, error) {
			tctx, cancel := context.WithTimeout(ctx, botReplyTimeout)
			defer cancel()
			return s.runTurnText(tctx, provider, "dingtalk-"+sessionKey, text)
		}
		return platform.NewDingTalkBot(pb.Name, pb.ClientID, pb.ClientSecret, reply, s.logf), nil
	default:
		return nil, fmt.Errorf("不支持的平台类型 %q", pb.Type)
	}
}

// botStatuses returns the live connection state of each running bot, keyed by
// name, so the API can annotate the configured list.
func (s *Server) botStatuses() map[string]platform.Status {
	s.bots.mu.Lock()
	defer s.bots.mu.Unlock()
	out := make(map[string]platform.Status, len(s.bots.bots))
	for _, b := range s.bots.bots {
		st := b.Status()
		out[st.Name] = st
	}
	return out
}

// runTurnText runs one non-streaming engine turn for the given session id and
// returns the assistant's final text. It is the non-SSE sibling of
// handleChatStream: same BuildAgent → NewRunner → Run sequence, but it
// accumulates the final (non-partial) text instead of streaming deltas, and
// reuses or creates a deterministic session so multi-turn context persists.
func (s *Server) runTurnText(ctx context.Context, provider, sessionID, text string) (string, error) {
	eng := s.engine()
	a, _, _, search, err := eng.BuildAgent(provider)
	if err != nil {
		return "", err
	}
	if search != nil {
		defer search.Close()
	}
	r2, svc, err := eng.NewRunner(a, search)
	if err != nil {
		return "", err
	}

	if resp, err := svc.Get(ctx, &adksession.GetRequest{AppName: engine.AppName, UserID: engine.UserID, SessionID: sessionID}); err != nil || resp.Session == nil {
		if _, err := svc.Create(ctx, &adksession.CreateRequest{AppName: engine.AppName, UserID: engine.UserID, SessionID: sessionID}); err != nil {
			return "", fmt.Errorf("create session: %w", err)
		}
	}

	msg := genai.NewContentFromText(text, genai.RoleUser)
	var sb strings.Builder
	for ev, runErr := range r2.Run(ctx, engine.UserID, sessionID, msg, agent.RunConfig{}) {
		if runErr != nil {
			return "", runErr
		}
		if ev == nil || ev.Content == nil || ev.Partial {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				sb.WriteString(p.Text)
			}
		}
	}

	indexSession(ctx, svc, search, sessionID)
	return sb.String(), nil
}

// platformInput is the body for POST /api/platforms (create or update).
type platformInput struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"` // empty on update keeps the stored secret
	Provider     string `json:"provider"`
	Enabled      bool   `json:"enabled"`
}

// handleListPlatforms lists configured platform bots with their live connection
// state. The client secret is never sent — only whether one is set.
func (s *Server) handleListPlatforms(w http.ResponseWriter, _ *http.Request) {
	type platformDTO struct {
		Name      string `json:"name"`
		Type      string `json:"type"`
		ClientID  string `json:"client_id,omitempty"`
		HasSecret bool   `json:"has_secret"`
		Provider  string `json:"provider,omitempty"`
		Enabled   bool   `json:"enabled"`
		State     string `json:"state"`            // online | connecting | error | stopped
		Detail    string `json:"detail,omitempty"` // error message when state == error
	}
	bots := s.engine().Config().Platforms
	statuses := s.botStatuses()
	out := make([]platformDTO, 0, len(bots))
	for _, b := range bots {
		d := platformDTO{
			Name: b.Name, Type: b.Type, ClientID: b.ClientID,
			HasSecret: b.ClientSecret != "", Provider: b.Provider,
			Enabled: b.Enabled, State: string(platform.StateStopped),
		}
		if st, ok := statuses[b.Name]; ok {
			d.State, d.Detail = string(st.State), st.Detail
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"platforms": out})
}

// handleSavePlatform upserts a platform bot and hot-reloads (which restarts the
// bots). An empty client_secret on update keeps the stored one, so editing an
// unrelated field never drops the credential.
func (s *Server) handleSavePlatform(w http.ResponseWriter, r *http.Request) {
	var in platformInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(in.Name)
	typ := strings.ToLower(strings.TrimSpace(in.Type))
	if typ == "" {
		typ = "dingtalk"
	}
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	if typ != "dingtalk" {
		writeErr(w, http.StatusBadRequest, "目前仅支持 dingtalk")
		return
	}

	path, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := loadRawOrEmpty(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	bot := config.PlatformBot{
		Name: name, Type: typ,
		ClientID:     strings.TrimSpace(in.ClientID),
		ClientSecret: in.ClientSecret,
		Provider:     strings.TrimSpace(in.Provider),
		Enabled:      in.Enabled,
	}
	if idx := indexOfPlatform(raw.Platforms, name); idx >= 0 {
		if strings.TrimSpace(bot.ClientSecret) == "" {
			bot.ClientSecret = raw.Platforms[idx].ClientSecret // keep stored secret
		}
		raw.Platforms[idx] = bot
	} else {
		if bot.ClientID == "" || strings.TrimSpace(bot.ClientSecret) == "" {
			writeErr(w, http.StatusBadRequest, "新建钉钉机器人需填写 client_id 与 client_secret")
			return
		}
		raw.Platforms = append(raw.Platforms, bot)
	}

	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}

// handleDeletePlatform removes a platform bot and hot-reloads.
func (s *Server) handleDeletePlatform(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := config.LoadRaw(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeErr(w, http.StatusNotFound, "尚无配置文件可删除")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := indexOfPlatform(raw.Platforms, name)
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "平台不存在")
		return
	}
	raw.Platforms = append(raw.Platforms[:idx], raw.Platforms[idx+1:]...)
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}

func indexOfPlatform(ps []config.PlatformBot, name string) int {
	for i, p := range ps {
		if p.Name == name {
			return i
		}
	}
	return -1
}
