package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/platform"

	"github.com/jelly-agent/jelly-agent/internal/logging"
)

// botReplyTimeout bounds one platform-driven engine turn, so a stuck model can't
// pin a bot's message handler forever.
const botReplyTimeout = 3 * time.Minute

// botManager owns the running platform bots. Bots are long-lived outbound
// connections (e.g. DingTalk Stream), not request-scoped, so they live here and
// are restarted whenever config reloads — mirroring how Watch and the MCP
// toolset cache are managed.
type botManager struct {
	mu sync.Mutex
	// ctx is the server lifecycle context, set by StartBots and cleared by
	// stopBots. Cleared, so a restart that arrives after shutdown builds
	// nothing rather than starting a set nobody will ever stop.
	ctx     context.Context
	stopped bool
	bots    []platform.Bot
	// newBot builds one bot, or nil for the real one. A field so a test can
	// hand back a double and, in doing so, control the moment a bot is
	// built — which is the window this file's ordering exists to close.
	newBot func(config.PlatformBot) (platform.Bot, error)
}

// StartBots launches the configured platform bots and keeps them running until
// ctx is cancelled. Called once from each server entrypoint, alongside Watch.
func (s *Server) StartBots(ctx context.Context) {
	// The same lock reload takes: startup and a config-file edit noticed at
	// the same moment are two writers to the same set of bots.
	s.restartMu.Lock()
	s.bots.mu.Lock()
	s.bots.ctx = ctx
	s.bots.mu.Unlock()

	s.restartBots(s.engine().Config())
	s.restartMu.Unlock()

	go func() {
		<-ctx.Done()
		s.stopBots()
	}()
}

// restartBots stops any running bots and starts the enabled ones from cfg. Safe
// to call on every config reload; a no-op until StartBots has supplied a ctx.
//
// Callers hold s.restartMu. It stops, builds and stores in three steps, and
// two of these running at once each store their own set while the other's
// bots keep running — see reload.
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

	// Built, then published, then started — in that order.
	//
	// Starting before publishing leaves a window where the manager's list is
	// empty and a bot is already on its way up: a shutdown landing there
	// stops nothing, and the connection comes up afterwards with no one
	// holding it. The WeChat bot makes that permanent — its Start ignores
	// the context it is given and runs on a background one, so cancelling
	// the server's context does not reach it either. Publishing first means
	// a Stop can always find it, and managedBot turns "found before it
	// started" into "does not start".
	type pending struct {
		bot  *managedBot
		name string
	}
	var built []pending
	var started []platform.Bot
	for _, pb := range cfg.Platforms {
		if !pb.Enabled {
			continue
		}
		build := s.buildBot
		if s.bots.newBot != nil {
			build = s.bots.newBot
		}
		bot, err := build(pb)
		if err != nil {
			slog.Warn("平台已跳过", "platform", pb.Name, logging.Err(err))
			continue
		}
		m := &managedBot{Bot: bot}
		built = append(built, pending{m, pb.Name})
		started = append(started, m)
	}

	s.bots.mu.Lock()
	if s.bots.stopped {
		// The server shut down while these were being built. Publishing them
		// now would put a set nobody is going to stop into the manager.
		s.bots.mu.Unlock()
		return
	}
	s.bots.bots = started
	s.bots.mu.Unlock()

	for _, p := range built {
		go p.bot.run(ctx, p.name)
	}
}

// stopBots disconnects all running bots and marks the manager shut down, so
// a restart already in flight does not publish a set after it.
func (s *Server) stopBots() {
	s.bots.mu.Lock()
	old := s.bots.bots
	s.bots.bots = nil
	s.bots.stopped = true
	s.bots.ctx = nil
	s.bots.mu.Unlock()
	for _, b := range old {
		b.Stop()
	}
}

// buildBot constructs one bot from its config, wiring a ReplyFunc that runs an
// engine turn keyed by the conversation.
func (s *Server) buildBot(pb config.PlatformBot) (platform.Bot, error) {
	provider := pb.Provider
	// mcpNames is the bot's selected MCP servers; a non-nil (possibly empty) slice
	// means "load exactly these" — so a bot with no selection gets no MCP tools.
	mcpNames := pb.MCP
	if mcpNames == nil {
		mcpNames = []string{}
	}
	// replyWith / streamReplyWith build per-platform reply funcs that run an
	// engine turn under a session id prefixed by platform (so the same person on
	// different platforms is kept in separate conversations).
	replyWith := func(prefix string) platform.ReplyFunc {
		return func(ctx context.Context, sessionKey, text string) (string, error) {
			tctx, cancel := context.WithTimeout(ctx, botReplyTimeout)
			defer cancel()
			return s.runTurnText(tctx, provider, mcpNames, prefix+sessionKey, text)
		}
	}
	streamReplyWith := func(prefix string) platform.StreamReplyFunc {
		return func(ctx context.Context, sessionKey, text string, onUpdate func(string)) (string, error) {
			tctx, cancel := context.WithTimeout(ctx, botReplyTimeout)
			defer cancel()
			return s.runTurnStream(tctx, provider, mcpNames, prefix+sessionKey, text, onUpdate)
		}
	}

	// Scope the logger with the platform's name once, so every line a bot
	// emits is attributable without the bot formatting its own name in.
	botLog := slog.With("platform", pb.Name, "type", pb.Type)

	switch pb.Type {
	case "dingtalk":
		if pb.ClientID == "" || pb.ClientSecret == "" {
			return nil, fmt.Errorf("dingtalk 需要 client_id 与 client_secret")
		}
		return platform.NewDingTalkBot(pb.Name, pb.ClientID, pb.ClientSecret, pb.Settings["card_template_id"], streamReplyWith("dingtalk-"), botLog), nil
	case "wechatpadpro":
		if pb.Settings["wechatpad_url"] == "" || pb.Settings["wechatpad_ws"] == "" {
			return nil, fmt.Errorf("wechatpadpro 需要 wechatpad_url 与 wechatpad_ws")
		}
		if pb.Settings["admin_key"] == "" && pb.Settings["token"] == "" {
			return nil, fmt.Errorf("wechatpadpro 需要 admin_key 或 token")
		}
		return platform.NewWeChatPadProBot(pb.Name, pb.Settings, replyWith("wechat-"), botLog), nil
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

// runTurnText runs one engine turn and returns only the final assistant text.
func (s *Server) runTurnText(ctx context.Context, provider string, mcpNames []string, sessionID, text string) (string, error) {
	return s.runTurnStream(ctx, provider, mcpNames, sessionID, text, nil)
}

// runTurnStream runs one engine turn for the given session id, streaming partial
// text to onUpdate (called with the accumulated text as it grows; may be nil)
// and returning the final full text. It is the platform-facing sibling of
// handleChatStream: same BuildAgent → NewRunner → Run (SSE) sequence, reusing or
// creating a deterministic session so multi-turn context persists.
func (s *Server) runTurnStream(ctx context.Context, provider string, mcpNames []string, sessionID, text string, onUpdate func(string)) (string, error) {
	eng, unpin := s.pin() // a config save must not close this engine mid-turn
	defer unpin()
	a, _, _, search, err := eng.BuildAgentWith(provider, mcpNames)
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

	// Same shape as the scheduler's: the session name is the identity of the
	// chat this bot is answering, so it is created under that name rather than
	// renamed. One implementation now, in chat.go.
	if err := ensureSession(ctx, svc, sessionID); err != nil {
		return "", err
	}

	msg := genai.NewContentFromText(text, genai.RoleUser)
	var deltas strings.Builder // accumulated partial text (the streamed answer)
	var finalText string       // full text from the final event (non-streaming fallback)
	for ev, runErr := range r2.Run(ctx, engine.UserID, sessionID, msg, agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) {
		if runErr != nil {
			return "", runErr
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		if ev.Partial {
			grew := false
			for _, p := range ev.Content.Parts {
				if p != nil && !p.Thought && p.Text != "" {
					deltas.WriteString(p.Text)
					grew = true
				}
			}
			if grew && onUpdate != nil {
				onUpdate(deltas.String())
			}
			continue
		}
		// Final aggregated event: capture its text as a fallback for models that
		// don't stream partials (it duplicates the deltas otherwise, so it's only
		// used when no partials arrived).
		var fb strings.Builder
		for _, p := range ev.Content.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				fb.WriteString(p.Text)
			}
		}
		if fb.Len() > 0 {
			finalText = fb.String()
		}
	}

	indexSession(ctx, svc, search, sessionID)
	if deltas.Len() > 0 {
		return deltas.String(), nil
	}
	return finalText, nil
}

// platformInput is the body for POST /api/platforms (create or update).
type platformInput struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	ClientID     string            `json:"client_id"`
	ClientSecret string            `json:"client_secret"` // empty on update keeps the stored secret
	Provider     string            `json:"provider"`
	Enabled      bool              `json:"enabled"`
	Settings     map[string]string `json:"settings,omitempty"` // platform-specific (wechatpadpro)
	MCP          []string          `json:"mcp,omitempty"`      // MCP servers this bot loads
}

// handleListPlatforms lists configured platform bots with their live connection
// state. The client secret is never sent — only whether one is set.
func (s *Server) handleListPlatforms(w http.ResponseWriter, r *http.Request) {
	type platformDTO struct {
		Name       string            `json:"name"`
		Type       string            `json:"type"`
		ClientID   string            `json:"client_id,omitempty"`
		HasSecret  bool              `json:"has_secret"`
		Provider   string            `json:"provider,omitempty"`
		Enabled    bool              `json:"enabled"`
		Settings   map[string]string `json:"settings,omitempty"`    // non-secret platform settings
		SecretKeys []string          `json:"secret_keys,omitempty"` // secret settings that are set
		MCP        []string          `json:"mcp,omitempty"`         // MCP servers this bot loads
		State      string            `json:"state"`                 // online | connecting | error | stopped
		Detail     string            `json:"detail,omitempty"`      // error message when state == error
		QR         string            `json:"qr,omitempty"`          // login QR (data URI) while awaiting scan
	}
	bots := s.engineFor(r).Config().Platforms
	statuses := s.botStatuses()
	out := make([]platformDTO, 0, len(bots))
	for _, b := range bots {
		visible, secretKeys := splitSettings(b.Settings)
		d := platformDTO{
			Name: b.Name, Type: b.Type, ClientID: b.ClientID,
			HasSecret: b.ClientSecret != "", Provider: b.Provider,
			Enabled: b.Enabled, Settings: visible, SecretKeys: secretKeys, MCP: b.MCP,
			State: string(platform.StateStopped),
		}
		if st, ok := statuses[b.Name]; ok {
			d.State, d.Detail, d.QR = string(st.State), st.Detail, st.QR
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
	if typ != "dingtalk" && typ != "wechatpadpro" {
		writeErr(w, http.StatusBadRequest, "目前支持 dingtalk / wechatpadpro")
		return
	}

	path, raw, done, err := s.editConfig()
	defer done()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	idx := indexOfPlatform(raw.Platforms, name)
	var existing config.PlatformBot
	if idx >= 0 {
		existing = raw.Platforms[idx]
	}
	bot := config.PlatformBot{Name: name, Type: typ, Provider: strings.TrimSpace(in.Provider), Enabled: in.Enabled, MCP: in.MCP}

	switch typ {
	case "dingtalk":
		bot.ClientID = strings.TrimSpace(in.ClientID)
		bot.ClientSecret = in.ClientSecret
		if strings.TrimSpace(bot.ClientSecret) == "" {
			bot.ClientSecret = existing.ClientSecret // keep stored secret
		}
		if idx < 0 && (bot.ClientID == "" || strings.TrimSpace(bot.ClientSecret) == "") {
			writeErr(w, http.StatusBadRequest, "新建钉钉机器人需填写 client_id 与 client_secret")
			return
		}
		// card_template_id (optional, enables streaming AI-card replies) lives in
		// Settings; empty submitted value keeps the stored one.
		bot.Settings = mergeSecrets(existing.Settings, in.Settings)
	case "wechatpadpro":
		// Empty submitted values keep the stored ones (secrets survive edits).
		bot.Settings = mergeSecrets(existing.Settings, in.Settings)
		if bot.Settings["wechatpad_url"] == "" || bot.Settings["wechatpad_ws"] == "" || (bot.Settings["admin_key"] == "" && bot.Settings["token"] == "") {
			writeErr(w, http.StatusBadRequest, "微信需填写 wechatpad_url、wechatpad_ws 与 admin_key（或 token）")
			return
		}
	}

	if idx >= 0 {
		raw.Platforms[idx] = bot
	} else {
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
	path, raw, done, err := s.editExistingConfig()
	defer done()
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

// splitSettings separates platform settings into values safe to echo back
// (url/ws/wxid) and the names of secret-ish keys (admin_key/token) that are set
// — so the API can show what's configured without ever returning a secret.
func splitSettings(m map[string]string) (visible map[string]string, secretKeys []string) {
	for k, v := range m {
		if isSecretSettingKey(k) {
			if strings.TrimSpace(v) != "" {
				secretKeys = append(secretKeys, k)
			}
			continue
		}
		if visible == nil {
			visible = map[string]string{}
		}
		visible[k] = v
	}
	sort.Strings(secretKeys)
	return visible, secretKeys
}

func isSecretSettingKey(k string) bool {
	k = strings.ToLower(k)
	return strings.Contains(k, "key") || strings.Contains(k, "secret") || strings.Contains(k, "token")
}

func indexOfPlatform(ps []config.PlatformBot, name string) int {
	for i, p := range ps {
		if p.Name == name {
			return i
		}
	}
	return -1
}

// managedBot makes Stop reliable whatever moment it arrives in.
//
// Start is asynchronous — connecting to DingTalk takes a round trip, and the
// server does not wait for it — so a restart's Stop can land before the bot
// has connected. Both implementations only have something to close once
// Start has installed it (the Stream client, the run goroutine's cancel), so
// that Stop finds nothing, returns, and the connection comes up a moment
// later with nobody holding it: an orphan that keeps answering the group
// chat, alongside its replacement, until the process exits. Two saves in the
// console produce it.
//
// So the intent to stop is remembered rather than acted on once. A Stop
// before Start cancels the start; a Stop during it is applied again when
// Start returns, which is the first moment there is anything to close.
//
// Here rather than in each bot: the manager is what knows a bot is being
// replaced, and the two implementations would otherwise both need the same
// state for the same reason.
type managedBot struct {
	platform.Bot

	mu      sync.Mutex
	stopped bool
	running bool
}

func (m *managedBot) run(ctx context.Context, name string) {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return // stopped before it ever started; nothing to connect
	}
	m.running = true
	m.mu.Unlock()

	err := m.Bot.Start(ctx)

	m.mu.Lock()
	m.running = false
	stopped := m.stopped
	m.mu.Unlock()
	if err != nil {
		slog.Error("平台启动失败", "platform", name, logging.Err(err))
		return
	}
	if stopped {
		// Stop came while this was connecting, and found nothing to close.
		// Now there is.
		m.Bot.Stop()
	}
}

func (m *managedBot) Stop() {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	m.Bot.Stop() // no-op if Start has not installed anything yet; run() redoes it
}
