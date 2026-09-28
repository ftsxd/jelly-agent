package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/execution"
	"github.com/jelly-agent/jelly-agent/internal/sandbox"
	adkmodel "google.golang.org/adk/model"
	adksession "google.golang.org/adk/session"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"
)

func TestConfirmationUsesOnlyStoredExactCall(t *testing.T) {
	a := execution.Approval{ID: "approval_a", SessionID: "s", Agent: "ops", InvocationID: "round", CallID: "c", Request: execution.Request{Command: "printf ok", Purpose: "测试", Profile: "local"}}
	makeEvent := func(command string) *adksession.Event {
		return &adksession.Event{InvocationID: "round", Author: "ops", LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			ID: "wrapper", Name: toolconfirmation.FunctionCallName, Args: map[string]any{
				"originalFunctionCall": &genai.FunctionCall{ID: "c", Name: "shell_exec", Args: map[string]any{"command": command, "purpose": "测试", "profile": "local"}},
				"toolConfirmation":     toolconfirmation.ToolConfirmation{Payload: map[string]any{"approval_id": "approval_a"}},
			},
		}}}}}}
	}
	e := makeEvent(a.Request.Command)
	msg, err := confirmationFromEvents([]*adksession.Event{e}, a, true)
	if err != nil || msg.Parts[0].FunctionResponse.ID != "wrapper" {
		t.Fatal(msg, err)
	}
	if _, err := confirmationFromEvents([]*adksession.Event{makeEvent("printf changed")}, a, true); err == nil {
		t.Fatal("changed command")
	}
	e.Author = "other"
	if _, err := confirmationFromEvents([]*adksession.Event{e}, a, true); err == nil {
		t.Fatal("other agent")
	}
	e.Author = "ops"
	answered := &adksession.Event{LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{FunctionResponse: msg.Parts[0].FunctionResponse}}}}}
	if _, err := confirmationFromEvents([]*adksession.Event{e, answered}, a, true); err == nil {
		t.Fatal("already answered confirmation")
	}
}

func TestWriteApprovalRoundTripAfterRestart(t *testing.T) {
	if !sandbox.OSSandboxAvailable() {
		t.Skip("OS sandbox required")
	}
	for _, scenario := range []struct{ approve, multi bool }{{true, false}, {false, false}, {true, true}} {
		approve, multi := scenario.approve, scenario.multi
		t.Run(fmt.Sprintf("approve=%t/multi=%t", approve, multi), func(t *testing.T) {
			var calls atomic.Int32
			var observed atomic.Value
			req := execution.Request{Command: "printf APPROVAL_TEST_OUTPUT && sleep 0.2", Purpose: "验证本地审批流程", Profile: "local"}
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				observed.Store(string(body))
				n := calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				var delta map[string]any
				finish := "stop"
				shellTurn := int32(1)
				if multi {
					shellTurn = 2
				}
				if multi && n == 1 {
					finish = "tool_calls"
					delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "transfer", "type": "function", "function": map[string]any{"name": "transfer_to_agent", "arguments": `{"agent_name":"ops"}`}}}}
				} else if n == shellTurn {
					args, _ := json.Marshal(req)
					finish = "tool_calls"
					delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "write_call", "type": "function", "function": map[string]any{"name": "shell_exec", "arguments": string(args)}}}}
				} else {
					delta = map[string]any{"content": "已处理审批"}
				}
				chunk, _ := json.Marshal(map[string]any{"id": "completion", "object": "chat.completion.chunk", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
			}))
			defer provider.Close()
			dir := t.TempDir()
			cfg := &config.Config{DefaultProvider: "test", Providers: []config.Provider{{Name: "test", BaseURL: provider.URL + "/v1", APIKey: "test", Model: "m"}}, Memory: config.Memory{Core: config.MemoryCore{Dir: filepath.Join(dir, "memory")}}, Execution: execution.Config{Enabled: true, Backend: "os", Profiles: []execution.Profile{{Name: "local", Agents: []string{"root"}, Network: true, WriteApproval: true}}}}
			if multi {
				cfg.Agents = []config.AgentDef{{Name: "coordinator", Enabled: true, SubAgents: []string{"ops"}}, {Name: "ops", Enabled: true}}
				cfg.DefaultAgent = "coordinator"
				cfg.Execution.Profiles[0].Agents = []string{"ops"}
			}
			makeServer := func() *Server {
				e := engine.New(cfg)
				e.SetStateRef(filepath.Join(dir, "state.db"))
				t.Cleanup(e.Close)
				return New(e, nil)
			}
			s := makeServer()
			w := do(t, s, "POST", "/api/chat/stream", `{"message":"请申请运行测试命令"}`)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			frames := parseSSE(t, w.Body.String())
			session := ""
			for _, f := range frames {
				if f["type"] == "session" {
					session, _ = f["session_id"].(string)
				}
				if f["type"] == "error" {
					t.Fatal(f)
				}
			}
			expectedCalls := int32(1)
			if multi {
				expectedCalls = 2
			}
			if session == "" || calls.Load() != expectedCalls {
				t.Fatalf("not paused: %s calls=%d", w.Body.String(), calls.Load())
			}
			db, err := s.engine().StateDB()
			if err != nil {
				t.Fatal(err)
			}
			store := execution.Approvals{DB: db}
			list, err := store.List(t.Context(), session, cfg.Execution)
			if err != nil || len(list) != 1 || list[0].State != "pending" {
				t.Fatal(list, err, w.Body.String())
			}
			a := list[0]
			if multi && (a.Agent != "ops" || a.Origin.Agent != "coordinator") {
				t.Fatal(a)
			}
			if w := do(t, s, "GET", "/api/sessions/"+session+"/timeline", ""); !strings.Contains(w.Body.String(), `"status":"waiting_input"`) {
				t.Fatal(w.Body.String())
			}
			// Resume with a newly built engine, not an in-memory pending map.
			s = makeServer()
			forged, _ := json.Marshal(map[string]any{"session_id": session, "approval_id": a.ID, "approve": approve, "command": "rm -rf /"})
			if w := do(t, s, "POST", "/api/chat/stream", string(forged)); w.Code != 400 {
				t.Fatal("replacement command accepted", w.Code, w.Body.String())
			}
			body, _ := json.Marshal(map[string]any{"session_id": session, "approval_id": a.ID, "approve": approve, "agent": "forged", "provider": "forged"})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- do(t, s, "POST", "/api/chat/stream", string(body)) }()
			running := false
			if approve {
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					if status, _ := s.runs().sessionStatus(session); status == TaskRunning {
						running = true
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			w = <-done
			if approve && !running {
				t.Fatal("approval execution was invisible while running")
			}
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			for _, f := range parseSSE(t, w.Body.String()) {
				if f["type"] == "error" {
					t.Fatal(f)
				}
			}
			got, _ := store.Get(t.Context(), a.ID)
			if approve {
				if got.State != "consumed" || got.Outcome != "succeeded" || got.ExecID == "" || !strings.Contains(observed.Load().(string), "APPROVAL_TEST_OUTPUT") {
					t.Fatal(got, w.Body.String(), observed.Load())
				}
			} else if got.State != "rejected" || got.ExecID != "" {
				t.Fatal(got)
			}
			if w := do(t, s, "POST", "/api/chat/stream", string(body)); w.Code != 409 {
				t.Fatal("duplicate approval", w.Code, w.Body.String())
			}
			// It continues the original task, so there is a single task entry.
			w = do(t, s, "GET", "/api/tasks", "")
			decoded := decode(t, w)
			tasks, _ := decoded["tasks"].([]any)
			if len(tasks) != 1 {
				t.Fatal(w.Body.String())
			}
			w = do(t, s, "GET", "/api/tasks/"+session+"/"+a.InvocationID, "")
			var detail struct {
				Task Task `json:"task"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
				t.Fatal(err)
			}
			resumed := []StepTool{}
			for _, step := range detail.Task.Steps {
				for _, tool := range step.Tools {
					if tool.Name == "shell_exec" && tool.Round != a.InvocationID {
						resumed = append(resumed, tool)
					}
				}
			}
			if len(resumed) != 1 || resumed[0].Pending || resumed[0].Args["command"] != req.Command {
				t.Fatal("resumed result missing from original task", w.Body.String())
			}
			if approve && (!resumed[0].OK || resumed[0].EvidenceID == "") {
				t.Fatal("approved result has no evidence", w.Body.String())
			}
			if !approve && resumed[0].OK {
				t.Fatal("denial shown as successful execution", w.Body.String())
			}
		})
	}
}

func TestApprovalEndpointValidationAndAuthentication(t *testing.T) {
	s := newTestServer(t)
	for _, body := range []string{`{"approval_id":"x","session_id":"s"}`, `{"approve":true}`, `{"approval_id":"x","approve":true}`, `{"approval_id":"x","approve":true,"session_id":"s","message":"changed"}`} {
		if w := do(t, s, "POST", "/api/chat/stream", body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := do(t, s, "POST", "/api/chat/stream", `{"approval_id":"unknown","approve":true,"session_id":"s"}`); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	admin := newAdminServer(t)
	for _, test := range []struct{ method, path, body string }{{"GET", "/api/sessions/s/approvals", ""}, {"POST", "/api/chat/stream", `{"session_id":"s","approval_id":"a","approve":true}`}} {
		if w := do(t, admin, test.method, test.path, test.body); w.Code != http.StatusUnauthorized {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestSessionClaimPreventsInterleavingBeforeFirstEvent(t *testing.T) {
	r := newRunRegistry()
	release, ok := r.claimSession("s")
	if !ok {
		t.Fatal("not claimed")
	}
	if _, ok := r.claimSession("s"); ok {
		t.Fatal("duplicate claim")
	}
	release()
	release()
	release, ok = r.claimSession("s")
	if !ok {
		t.Fatal("claim leaked")
	}
	release()
	finish := r.start("s", "round", "", func() {})
	if _, ok := r.claimSession("s"); ok {
		t.Fatal("running session claimed")
	}
	finish(TaskCompleted)
}

func TestApprovalWaitingStatusIsDurableAndExpiryBlocks(t *testing.T) {
	s := newTestServer(t)
	cfg := s.engine().Config()
	cfg.Execution = execution.Config{Enabled: true, Profiles: []execution.Profile{{Name: "local", Agents: []string{"root"}, WriteApproval: true}}}
	db, err := s.engine().StateDB()
	if err != nil {
		t.Fatal(err)
	}
	store := execution.Approvals{DB: db}
	a, err := store.Create(context.Background(), cfg.Execution, "root", "s", "round", "c", execution.Request{Command: "printf ok", Purpose: "test", Profile: "local"})
	if err != nil {
		t.Fatal(err)
	}
	frames := []map[string]any{{"type": "tool_call", "round": "round", "name": "shell_exec", "call_id": "c", "ts": int64(1)}, {"type": "tool_result", "round": "round", "name": "shell_exec", "call_id": "c", "ok": true}}
	info := func(string) ToolInfo { return ToolInfo{Known: true, Mutating: true} }
	tasks := s.foldWithStatus(t.Context(), s.engine(), "s", frames, info, nil)
	if tasks[0].Status != TaskWaitingInput {
		t.Fatal(tasks)
	}
	db.Exec(`UPDATE execution_approvals SET expires_ms=? WHERE id=?`, time.Now().Add(-time.Second).UnixMilli(), a.ID)
	tasks = s.foldWithStatus(t.Context(), s.engine(), "s", frames, info, nil)
	if tasks[0].Status != TaskBlocked {
		t.Fatal(tasks)
	}
}
