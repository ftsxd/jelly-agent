package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/execution"
	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

type approvalPauseBody struct {
	*strings.Reader
	entered, resume chan struct{}
	once            sync.Once
}

func (b *approvalPauseBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.resume })
	return b.Reader.Read(p)
}
func (b *approvalPauseBody) Close() error { return nil }

func TestApprovalRequestPinnedBeforeRevocation(t *testing.T) {
	if !sandbox.OSSandboxAvailable() {
		t.Skip("OS sandbox required")
	}
	var calls atomic.Int32
	req := execution.Request{Command: "printf AUDIT_REVOCATION", Purpose: "audit local print", Profile: "local"}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		finish := "stop"
		delta := map[string]any{"content": "finished"}
		if n == 1 {
			args, _ := json.Marshal(req)
			finish = "tool_calls"
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "write_call", "type": "function", "function": map[string]any{"name": "shell_exec", "arguments": string(args)}}}}
		}
		chunk, _ := json.Marshal(map[string]any{"id": "completion", "object": "chat.completion.chunk", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer provider.Close()
	dir := t.TempDir()
	cfg := &config.Config{DefaultProvider: "test", Providers: []config.Provider{{Name: "test", BaseURL: provider.URL + "/v1", APIKey: "test", Model: "m"}}, Memory: config.Memory{Core: config.MemoryCore{Dir: filepath.Join(dir, "memory")}}, Execution: execution.Config{Enabled: true, Backend: "os", Profiles: []execution.Profile{{Name: "local", Agents: []string{"root"}, Network: true, WriteApproval: true}}}}
	old := engine.New(cfg)
	old.SetStateRef(filepath.Join(dir, "state.db"))
	t.Cleanup(old.Close)
	s := New(old, nil)
	first := do(t, s, "POST", "/api/chat/stream", `{"message":"audit request"}`)
	var session string
	for _, f := range parseSSE(t, first.Body.String()) {
		if f["type"] == "session" {
			session, _ = f["session_id"].(string)
		}
	}
	db, _ := old.StateDB()
	store := execution.Approvals{DB: db}
	pending, err := store.List(t.Context(), session, cfg.Execution)
	if err != nil || len(pending) != 1 {
		t.Fatal(err, pending, first.Body.String())
	}
	body, _ := json.Marshal(map[string]any{"session_id": session, "approval_id": pending[0].ID, "approve": true})
	pause := &approvalPauseBody{Reader: strings.NewReader(string(body)), entered: make(chan struct{}), resume: make(chan struct{})}
	request := httptest.NewRequest("POST", "/api/chat/stream", pause)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.Handler().ServeHTTP(response, request); close(done) }()
	<-pause.entered // middleware has pinned old engine, before body decode
	revoked := *cfg
	revoked.Execution = execution.Config{}
	fresh := engine.New(&revoked)
	fresh.SetStateRef(filepath.Join(dir, "state.db"))
	t.Cleanup(fresh.Close)
	s.mu.Lock()
	s.ref = &engineRef{eng: fresh}
	s.mu.Unlock()
	close(pause.resume)
	<-done
	got, _ := store.Get(t.Context(), pending[0].ID)
	t.Logf("after revocation HTTP=%d approval=%s outcome=%s exec=%s", response.Code, got.State, got.Outcome, got.ExecID)
	if got.State == "consumed" {
		t.Fatalf("revoked config still executed: %s", response.Body.String())
	}
}
