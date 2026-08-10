package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	adkmemory "google.golang.org/adk/memory"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/memory"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	jellytool "github.com/jelly-agent/jelly-agent/internal/tool"
)

// handleProviders lists configured providers with masked API keys, flagging the
// default. Powers the Chat view's provider picker.
func (s *Server) handleProviders(w http.ResponseWriter, _ *http.Request) {
	cfg := s.engine().Config()
	type providerDTO struct {
		Name      string `json:"name"`
		BaseURL   string `json:"base_url"`
		Model     string `json:"model"`
		APIKey    string `json:"api_key"` // masked
		IsDefault bool   `json:"is_default"`

		// Tuning fields; null ⇒ unset, i.e. the model layer's default applies.
		Temperature *float64 `json:"temperature"`
		MaxTokens   int      `json:"max_tokens"`
		TimeoutSec  int      `json:"timeout_sec"`
		MaxRetries  *int     `json:"max_retries"`
	}
	out := make([]providerDTO, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		out = append(out, providerDTO{
			Name:        p.Name,
			BaseURL:     p.BaseURL,
			Model:       p.Model,
			APIKey:      config.MaskKey(p.APIKey),
			IsDefault:   p.Name == cfg.DefaultProvider,
			Temperature: p.Temperature,
			MaxTokens:   p.MaxTokens,
			TimeoutSec:  p.TimeoutSec,
			MaxRetries:  p.MaxRetries,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"default":   cfg.DefaultProvider,
		"providers": out,
	})
}

// handleTools lists the built-in tools the agent would expose, mirroring the
// CLI's `jelly tool list` / `/tools`.
func (s *Server) handleTools(w http.ResponseWriter, _ *http.Request) {
	core, err := s.engine().Core()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tools, err := s.engine().Tools(core, s.engine().SearchEnabled())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type toolDTO struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	out := make([]toolDTO, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolDTO{Name: t.Name(), Description: t.Description()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out})
}

// handleToolFetch runs fetch_url directly (bypassing the model) so the Tools
// view can check what a page actually reduces to before an agent relies on it.
// Body: {"url": "https://…", "max_chars": 8000}.
//
// The tool's own guards still apply — scheme check, the private-address dialer
// guard, size and redirect caps — so this endpoint is no more reachable into
// the local network than the agent is.
func (s *Server) handleToolFetch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := jellytool.FetchURL(r.Context(), req.URL, req.MaxChars)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleToolTest runs web_search directly (bypassing the model) so the Tools
// view can exercise the search backend. Body: {"query": "...", "max": 5}.
func (s *Server) handleToolTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
		Max   int    `json:"max"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := jellytool.Search(r.Context(), req.Query, req.Max)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// sessionDTO is one row in the sessions list.
type sessionDTO struct {
	ID         string `json:"id"`
	Events     int    `json:"events"`
	LastUpdate int64  `json:"last_update"` // unix seconds
	Preview    string `json:"preview,omitempty"`
}

// handleSessions lists persisted sessions, newest first, with limit/offset
// pagination. It reads a lightweight projection (id + event count + last update)
// straight from the store, so listing stays cheap as history grows — and unlike
// ADK's session.List it reports the real per-session event count.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50, 1, 200)
	offset := queryInt(r, "offset", 0, 0, 1<<30)
	rows, total, err := jellysession.ListPage("", engine.AppName, engine.UserID, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	svc, err := s.engine().NewSessionService()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]sessionDTO, 0, len(rows))
	for _, m := range rows {
		out = append(out, sessionDTO{ID: m.ID, Events: m.Events, LastUpdate: m.LastUpdate, Preview: sessionPreview(r.Context(), svc, m.ID)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": out,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
		"has_more": offset+len(out) < total,
	})
}

// sessionPreview returns the first user text for human-friendly history labels.
// It is best-effort: a missing/corrupt session must not make the list unusable.
func sessionPreview(ctx context.Context, svc adksession.Service, id string) string {
	resp, err := svc.Get(ctx, &adksession.GetRequest{AppName: engine.AppName, UserID: engine.UserID, SessionID: id})
	if err != nil || resp.Session == nil {
		return ""
	}
	for ev := range resp.Session.Events().All() {
		if roleForAuthor(ev.Author) != "user" || ev.Content == nil {
			continue
		}
		var b strings.Builder
		for _, p := range ev.Content.Parts {
			if p != nil {
				b.WriteString(p.Text)
			}
		}
		text := strings.TrimSpace(b.String())
		if len([]rune(text)) > 42 {
			return string([]rune(text)[:42]) + "…"
		}
		if text != "" {
			return text
		}
	}
	return ""
}

// handleSessionIDs returns every session id (newest first) for the "select all
// across pages" action, so batch delete can target the full set without paging.
func (s *Server) handleSessionIDs(w http.ResponseWriter, _ *http.Request) {
	ids, err := jellysession.AllIDs("", engine.AppName, engine.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": ids, "total": len(ids)})
}

// queryInt parses a query parameter as an int, clamping to [min, max] and
// falling back to def when absent or unparseable.
func queryInt(r *http.Request, key string, def, min, max int) int {
	v := strings.TrimSpace(r.URL.Query().Get(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// eventDTO is one rendered transcript entry.
type eventDTO struct {
	Author      string        `json:"author"`
	Role        string        `json:"role"`
	Text        string        `json:"text,omitempty"`
	ToolCalls   []toolCallDTO `json:"tool_calls,omitempty"`
	ToolResults []toolResult  `json:"tool_results,omitempty"`
	Timestamp   int64         `json:"timestamp"`
}

type toolCallDTO struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type toolResult struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response,omitempty"`
}

// handleSessionDetail returns one session's transcript as structured turns.
func (s *Server) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc, err := s.engine().NewSessionService()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp, err := svc.Get(r.Context(), &adksession.GetRequest{AppName: engine.AppName, UserID: engine.UserID, SessionID: id})
	if err != nil || resp.Session == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}

	var (
		events                             []eventDTO
		promptTok, completionTok, totalTok int32
	)
	for ev := range resp.Session.Events().All() {
		events = append(events, eventToDTO(ev))
		if ev.UsageMetadata != nil {
			promptTok += ev.UsageMetadata.PromptTokenCount
			completionTok += ev.UsageMetadata.CandidatesTokenCount
			totalTok += ev.UsageMetadata.TotalTokenCount
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     resp.Session.ID(),
		"events": events,
		"usage": map[string]int32{
			"prompt":     promptTok,
			"completion": completionTok,
			"total":      totalTok,
		},
	})
}

// handleDeleteSession removes one persisted session (and its events) from the
// store. Deleting a missing session is treated as success (idempotent).
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := jellysession.DeleteSessions("", engine.AppName, engine.UserID, []string{id}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Also drop the session's L2 search-index rows, else load_memory could still
	// surface the deleted conversation.
	_, _ = memory.PurgeSessions("", []string{id})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteSessions removes several sessions in one request (batch delete on
// the Sessions page). Deleting a missing session is fine (idempotent); the first
// hard error aborts and reports how many were already removed.
func (s *Server) handleDeleteSessions(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(in.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "ids 不能为空")
		return
	}
	deleted, err := jellysession.DeleteSessions("", engine.AppName, engine.UserID, in.IDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("批量删除失败（已删除 %d 个）: %v", deleted, err))
		return
	}
	// Also drop the sessions' L2 search-index rows (see handleDeleteSession).
	_, _ = memory.PurgeSessions("", in.IDs)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted})
}

func eventToDTO(ev *adksession.Event) eventDTO {
	dto := eventDTO{Author: ev.Author, Role: roleForAuthor(ev.Author), Timestamp: ev.Timestamp.Unix()}
	if ev.Content == nil {
		return dto
	}
	var text strings.Builder
	for _, p := range ev.Content.Parts {
		switch {
		case p == nil:
			continue
		case p.Thought:
			continue // thinking parts stay out of the transcript
		case p.Text != "":
			text.WriteString(p.Text)
		case p.FunctionCall != nil:
			dto.ToolCalls = append(dto.ToolCalls, toolCallDTO{Name: p.FunctionCall.Name, Args: p.FunctionCall.Args})
		case p.FunctionResponse != nil:
			dto.ToolResults = append(dto.ToolResults, toolResult{Name: p.FunctionResponse.Name, Response: p.FunctionResponse.Response})
		}
	}
	dto.Text = text.String()
	return dto
}

// roleForAuthor maps an event author to a coarse UI role.
func roleForAuthor(author string) string {
	if author == string(genai.RoleUser) || author == "user" {
		return "user"
	}
	return "agent"
}

// handleMemoryCore returns the L1 core-memory snapshot (USER.md / MEMORY.md).
func (s *Server) handleMemoryCore(w http.ResponseWriter, _ *http.Request) {
	core, err := s.engine().Core()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	mem, usr := core.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"dir":            core.Dir(),
		"user":           usr,
		"memory":         mem,
		"search_enabled": s.engine().SearchEnabled(),
		"search_top_k":   s.engine().Config().Memory.Search.TopK,
	})
}

// memoryCoreInput sets one core-memory file's raw content from the web editor.
type memoryCoreInput struct {
	Target  string `json:"target"` // "user" | "memory"
	Content string `json:"content"`
}

// handleSetMemoryCore overwrites USER.md or MEMORY.md with edited content. The
// next agent turn reads it fresh (InstructionProvider), so it takes effect
// immediately — this is the manual counterpart to the agent's remember/forget.
func (s *Server) handleSetMemoryCore(w http.ResponseWriter, r *http.Request) {
	var in memoryCoreInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var target memory.Target
	switch in.Target {
	case "user":
		target = memory.TargetUser
	case "memory", "":
		target = memory.TargetMemory
	default:
		writeErr(w, http.StatusBadRequest, "target 仅支持 user / memory")
		return
	}
	core, err := s.engine().Core()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := core.Set(target, in.Content); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMemorySearch runs an L2 FTS5 query over indexed sessions. Returns an
// explicit disabled flag when L2 is off so the UI can guide the user.
func (s *Server) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	if !s.engine().SearchEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "results": []any{}})
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeErr(w, http.StatusBadRequest, "missing query parameter q")
		return
	}
	search, err := s.engine().Search()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer search.Close()

	resp, err := search.SearchMemory(r.Context(), &adkmemory.SearchRequest{
		Query:   query,
		AppName: engine.AppName,
		UserID:  engine.UserID,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type memoryHit struct {
		Author    string `json:"author"`
		Text      string `json:"text"`
		Timestamp int64  `json:"timestamp"`
	}
	hits := make([]memoryHit, 0, len(resp.Memories))
	for _, m := range resp.Memories {
		hits = append(hits, memoryHit{
			Author:    m.Author,
			Text:      memoryText(m),
			Timestamp: m.Timestamp.Unix(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "query": query, "results": hits})
}

// memoryText joins the text parts of a memory entry's content.
func memoryText(m adkmemory.Entry) string {
	if m.Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range m.Content.Parts {
		if p != nil && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
