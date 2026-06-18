package server

import (
	"net/http"
	"sort"
	"strings"

	adkmemory "google.golang.org/adk/memory"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
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
	}
	out := make([]providerDTO, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		out = append(out, providerDTO{
			Name:      p.Name,
			BaseURL:   p.BaseURL,
			Model:     p.Model,
			APIKey:    config.MaskKey(p.APIKey),
			IsDefault: p.Name == cfg.DefaultProvider,
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
}

// handleSessions lists persisted sessions, newest first.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	svc, err := s.engine().NewSessionService()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp, err := svc.List(r.Context(), &adksession.ListRequest{AppName: engine.AppName, UserID: engine.UserID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]sessionDTO, 0, len(resp.Sessions))
	for _, sess := range resp.Sessions {
		out = append(out, sessionDTO{
			ID:         sess.ID(),
			Events:     sess.Events().Len(),
			LastUpdate: sess.LastUpdateTime().Unix(),
		})
	}
	// List order is storage-defined; sort newest-first for the UI.
	sort.Slice(out, func(i, j int) bool { return out[i].LastUpdate > out[j].LastUpdate })
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
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
	svc, err := s.engine().NewSessionService()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := svc.Delete(r.Context(), &adksession.DeleteRequest{AppName: engine.AppName, UserID: engine.UserID, SessionID: id}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
