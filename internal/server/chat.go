package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/adk/agent"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/memory"
)

// chatRequest is the body of POST /api/chat/stream.
type chatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id,omitempty"` // empty = start a new session
	Provider  string `json:"provider,omitempty"`   // empty = default provider
	Agent     string `json:"agent,omitempty"`      // named multi-agent root; empty = default/legacy
}

// sessionSeq disambiguates web session ids created within the same nanosecond.
var sessionSeq atomic.Uint64

// handleChatStream runs one turn and streams structured events to the client as
// Server-Sent Events. Event shapes (the "type" field): "session" (the resolved
// id, sent first), "text_delta", "tool_call", "tool_result", "usage", "done",
// and "error". The frontend reads this over fetch + ReadableStream (EventSource
// can't POST).
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeErr(w, http.StatusBadRequest, "message 不能为空")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	eng := s.engine() // pin one engine for this whole turn
	// Pick a named agent tree when one is requested or configured by default;
	// otherwise fall back to the legacy single agent on the chosen provider.
	agentName := strings.TrimSpace(req.Agent)
	if agentName == "" && eng.HasAgents() {
		agentName = eng.DefaultAgentName()
	}
	var (
		a      agent.Agent
		search *memory.Search
		err    error
	)
	if agentName != "" {
		a, _, _, search, err = eng.BuildAgentByName(agentName)
	} else {
		a, _, _, search, err = eng.BuildAgent(req.Provider)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if search != nil {
		defer search.Close()
	}
	r2, svc, err := eng.NewRunner(a, search)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	ctx := r.Context()
	sessionID, err := s.resolveSession(ctx, svc, req.SessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Past this point the response is an SSE stream; errors go in-band.
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
	w.WriteHeader(http.StatusOK)

	sse := &sseWriter{w: w, flusher: flusher}
	sse.send("session", map[string]any{"session_id": sessionID})

	msg := genai.NewContentFromText(req.Message, genai.RoleUser)
	for ev, runErr := range r2.Run(ctx, engine.UserID, sessionID, msg, agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) {
		if runErr != nil {
			sse.send("error", map[string]any{"message": runErr.Error()})
			return
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		if ev.Partial {
			for _, p := range ev.Content.Parts {
				if p != nil && !p.Thought && p.Text != "" {
					sse.send("text_delta", map[string]any{"text": p.Text, "agent": ev.Author})
				}
			}
			continue
		}
		emitFinal(sse, ev)
	}

	// Index the just-finished turn into L2 so future searches see it.
	indexSession(ctx, svc, search, sessionID)
	sse.send("done", map[string]any{"session_id": sessionID})
}

// emitFinal forwards tool calls/results and token usage from a non-partial
// event. Text was already streamed via partials, so it is not re-sent.
func emitFinal(sse *sseWriter, ev *adksession.Event) {
	for _, p := range ev.Content.Parts {
		switch {
		case p == nil:
			continue
		case p.FunctionCall != nil:
			sse.send("tool_call", map[string]any{"name": p.FunctionCall.Name, "args": p.FunctionCall.Args, "agent": ev.Author})
		case p.FunctionResponse != nil:
			resp := p.FunctionResponse.Response
			sse.send("tool_result", map[string]any{
				"name": p.FunctionResponse.Name, "response": resp, "agent": ev.Author,
				"ok": !toolFailed(resp), "error": toolError(resp),
			})
		}
	}
	if ev.UsageMetadata != nil {
		u := ev.UsageMetadata
		sse.send("usage", map[string]any{
			"prompt":     u.PromptTokenCount,
			"completion": u.CandidatesTokenCount,
			"total":      u.TotalTokenCount,
		})
	}
}

// resolveSession returns an existing session id when the client supplied a known
// one, otherwise creates a fresh "web-..." session.
func (s *Server) resolveSession(ctx context.Context, svc adksession.Service, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id != "" {
		resp, err := svc.Get(ctx, &adksession.GetRequest{AppName: engine.AppName, UserID: engine.UserID, SessionID: id})
		if err == nil && resp.Session != nil {
			return id, nil
		}
	}
	fresh := newWebSessionID()
	if _, err := svc.Create(ctx, &adksession.CreateRequest{AppName: engine.AppName, UserID: engine.UserID, SessionID: fresh}); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return fresh, nil
}

// indexSession ingests the named session into L2 search. No-op when search is
// disabled; failures are swallowed (best-effort, the turn already succeeded).
func indexSession(ctx context.Context, svc adksession.Service, search *memory.Search, sessionID string) {
	if search == nil {
		return
	}
	resp, err := svc.Get(ctx, &adksession.GetRequest{AppName: engine.AppName, UserID: engine.UserID, SessionID: sessionID})
	if err != nil || resp.Session == nil {
		return
	}
	_ = search.AddSessionToMemory(ctx, resp.Session)
}

func newWebSessionID() string {
	return "web-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatUint(sessionSeq.Add(1), 10)
}

// sseWriter serializes structured events as SSE "data:" lines.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// send writes one event as `data: {"type":...,...}` and flushes immediately.
func (s *sseWriter) send(typ string, payload map[string]any) {
	payload["type"] = typ
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.flusher.Flush()
}

// decodeJSON decodes a request body into v, rejecting unknown fields.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}
