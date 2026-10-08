package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/execution"
	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

// One approval with "don't ask again" covers the next command of its class in
// the same session — run for real, in the sandbox — until it is revoked.
func TestSessionGrantSkipsTheNextApprovalOfItsClass(t *testing.T) {
	if !sandbox.OSSandboxAvailable() {
		t.Skip("OS sandbox required")
	}
	dir := t.TempDir()
	tools := filepath.Join(dir, "tools")
	if err := os.MkdirAll(filepath.Join(tools, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "bin", "tccli"), []byte("#!/bin/sh\necho FAKE_TCCLI \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Turn n of the fake model: odd turns stop an instance, even ones answer.
	var calls atomic.Int32
	var lastBody atomic.Value
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastBody.Store(string(body))
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		delta, finish := map[string]any{"content": "好的"}, "stop"
		if n%2 == 1 {
			args, _ := json.Marshal(execution.Request{Command: fmt.Sprintf("tccli cvm StopInstances --InstanceIds ins-%d", (n+1)/2), Purpose: "停机", Profile: "cloud"})
			finish = "tool_calls"
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call_%d", n), "type": "function", "function": map[string]any{"name": "shell_exec", "arguments": string(args)}}}}
		}
		chunk, _ := json.Marshal(map[string]any{"id": "c", "object": "chat.completion.chunk", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer provider.Close()

	cfg := &config.Config{DefaultProvider: "test", Providers: []config.Provider{{Name: "test", BaseURL: provider.URL + "/v1", APIKey: "test", Model: "m"}},
		Memory:    config.Memory{Core: config.MemoryCore{Dir: filepath.Join(dir, "memory")}},
		Execution: execution.Config{Enabled: true, Backend: "os", Profiles: []execution.Profile{{Name: "cloud", Agents: []string{"root"}, WriteApproval: true, ToolDir: tools}}}}
	e := engine.New(cfg)
	e.SetStateRef(filepath.Join(dir, "state.db"))
	t.Cleanup(e.Close)
	s := New(e, nil)

	chat := func(body string) []map[string]any {
		t.Helper()
		w := do(t, s, "POST", "/api/chat/stream", body)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		frames := parseSSE(t, w.Body.String())
		for _, f := range frames {
			if f["type"] == "error" {
				t.Fatal(f)
			}
		}
		return frames
	}
	type listing struct {
		Approvals []execution.Approval `json:"approvals"`
		Grants    []execution.Grant    `json:"grants"`
		Available bool                 `json:"grants_available"`
	}
	approvals := func(session string) listing {
		t.Helper()
		var l listing
		if err := json.Unmarshal(do(t, s, "GET", "/api/sessions/"+session+"/approvals", "").Body.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	pending := func(l listing) []execution.Approval {
		var out []execution.Approval
		for _, a := range l.Approvals {
			if a.State == "pending" {
				out = append(out, a)
			}
		}
		return out
	}

	// 1. ins-1 needs approval; the card offers the class.
	session := ""
	for _, f := range chat(`{"message":"停掉实例"}`) {
		if f["type"] == "session" {
			session, _ = f["session_id"].(string)
		}
	}
	l := approvals(session)
	if p := pending(l); len(p) != 1 || p[0].GrantClass != "tccli cvm StopInstances" || !l.Available {
		t.Fatalf("approval not offering the grant: %+v", l)
	}

	// 2. Approve with remember: ins-1 runs and the grant is in force.
	body, _ := json.Marshal(map[string]any{"session_id": session, "approval_id": pending(l)[0].ID, "approve": true, "remember": true})
	chat(string(body))
	if !strings.Contains(lastBody.Load().(string), "FAKE_TCCLI cvm StopInstances --InstanceIds ins-1") {
		t.Fatal("approved command did not run", lastBody.Load())
	}
	if l = approvals(session); len(l.Grants) != 1 || l.Grants[0].Class != "tccli cvm StopInstances" {
		t.Fatalf("grant missing: %+v", l)
	}

	// 3. ins-2 in the same session runs without a card.
	chat(`{"message":"再停一台","session_id":"` + session + `"}`)
	if p := pending(approvals(session)); len(p) != 0 {
		t.Fatalf("granted class asked again: %+v", p)
	}
	if !strings.Contains(lastBody.Load().(string), "FAKE_TCCLI cvm StopInstances --InstanceIds ins-2") {
		t.Fatal("granted command did not run", lastBody.Load())
	}

	// 4. Revoked: ins-3 is asked about again.
	if w := do(t, s, "DELETE", "/api/sessions/"+session+"/grants/"+l.Grants[0].ID, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	chat(`{"message":"第三台","session_id":"` + session + `"}`)
	if p := pending(approvals(session)); len(p) != 1 || !strings.Contains(p[0].Request.Command, "ins-3") {
		t.Fatalf("revoked grant still applied: %+v", p)
	}
}
