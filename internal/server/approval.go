package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/execution"
	"github.com/jelly-agent/jelly-agent/internal/tool"
	adksession "google.golang.org/adk/session"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"
)

func (s *Server) handleExecutionApprovals(w http.ResponseWriter, r *http.Request) {
	eng := s.engineFor(r)
	db, err := eng.StateDB()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	list, err := (execution.Approvals{DB: db}).List(r.Context(), r.PathValue("id"), eng.ExecutionConfig())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !s.runs().sessionActive(r.PathValue("id")) {
		for i := range list {
			if list[i].State == "approved" {
				list[i].State = "abandoned"
			}
		}
	}
	cfg := eng.ExecutionConfig()
	for i := range list {
		if list[i].State == "pending" {
			list[i].GrantClass = cfg.GrantClassFor(list[i].Agent, list[i].Request)
		}
	}
	// Grants are optional: a store without the table still lists approvals.
	grants, grantErr := (execution.Approvals{DB: db}).Grants(r.Context(), r.PathValue("id"), cfg)
	if grants == nil {
		grants = []execution.Grant{}
	}
	writeJSON(w, 200, map[string]any{"approvals": list, "grants": grants, "grants_available": grantErr == nil})
}

// handleRevokeGrant ends a session grant: later commands of its class are
// asked about again.
func (s *Server) handleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	db, err := s.engineFor(r).StateDB()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := (execution.Approvals{DB: db}).RevokeGrant(r.Context(), r.PathValue("id"), r.PathValue("grant")); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func sessionHasPendingApprovals(ctx context.Context, eng *engine.Engine, session string) bool {
	db, err := eng.StateDB()
	if err != nil {
		return false
	}
	states, err := (execution.Approvals{DB: db}).StatesForSessions(ctx, []string{session}, eng.ExecutionConfig())
	if err != nil {
		return false
	}
	for _, a := range states[session] {
		if a.State == "pending" {
			return true
		}
	}
	return false
}

// Build the ADK response from the stored wrapper. The browser cannot supply a
// different command, tool call id, payload, agent or credential source.
func confirmationMessage(ctx context.Context, svc adksession.Service, a execution.Approval, approve bool) (*genai.Content, error) {
	resp, err := svc.Get(ctx, &adksession.GetRequest{AppName: engine.AppName, UserID: engine.UserID, SessionID: a.SessionID})
	if err != nil || resp.Session == nil {
		return nil, execution.ErrApproval
	}
	events := []*adksession.Event{}
	for e := range resp.Session.Events().All() {
		events = append(events, e)
	}
	return confirmationFromEvents(events, a, approve)
}
func confirmationFromEvents(events []*adksession.Event, a execution.Approval, approve bool) (*genai.Content, error) {
	pending := map[string]bool{}
	for _, id := range pendingApprovals(events) {
		pending[id] = true
	}
	for _, e := range events {
		if e == nil || e.Content == nil || e.InvocationID != a.InvocationID || e.Author != a.Agent {
			continue
		}
		for _, p := range e.Content.Parts {
			if p == nil || p.FunctionCall == nil {
				continue
			}
			fc := p.FunctionCall
			if fc.Name != toolconfirmation.FunctionCallName || !pending[fc.ID] {
				continue
			}
			orig, err := toolconfirmation.OriginalCallFrom(fc)
			if err != nil || orig.Name != tool.ShellExecName || orig.ID != a.CallID {
				continue
			}
			var req execution.Request
			b, _ := json.Marshal(orig.Args)
			if json.Unmarshal(b, &req) != nil || req != a.Request {
				continue
			}
			var conf toolconfirmation.ToolConfirmation
			b, _ = json.Marshal(fc.Args["toolConfirmation"])
			if json.Unmarshal(b, &conf) != nil {
				continue
			}
			payload, ok := conf.Payload.(map[string]any)
			if !ok || payload["approval_id"] != a.ID {
				continue
			}
			return &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
				ID: fc.ID, Name: toolconfirmation.FunctionCallName, Response: map[string]any{"confirmed": approve, "payload": map[string]any{"approval_id": a.ID},
					"originalFunctionCall": orig, "original_agent": a.Agent, "original_branch": e.Branch},
			}}}}, nil
		}
	}
	return nil, execution.ErrApproval
}
