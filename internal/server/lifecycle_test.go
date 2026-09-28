package server

import (
	"encoding/json"
	"fmt"
	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/execution"
	"github.com/jelly-agent/jelly-agent/internal/sandbox"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInterruptedRunAfterRestartIsBlocked(t *testing.T) {
	s := newTestServer(t)
	frames := []map[string]any{{"type": "tool_call", "round": "round", "name": "shell_exec", "call_id": "c", "ts": int64(1)}}
	tasks := s.foldWithStatus(t.Context(), s.engine(), "session", frames, func(string) ToolInfo { return ToolInfo{Known: true, Mutating: true} }, nil)
	if len(tasks) != 1 {
		t.Fatal(tasks)
	}
	t.Logf("task=%s step=%s pending=%t", tasks[0].Status, tasks[0].Steps[0].Status, tasks[0].Steps[0].Tools[0].Pending)
	if tasks[0].Status == TaskCompleted || tasks[0].Status == TaskRunning {
		t.Fatal("unfinished run is still reported completed/running after restart")
	}
}

func TestHandlerStopRecordsCancelled(t *testing.T) {
	if !sandbox.OSSandboxAvailable() {
		t.Skip("OS sandbox required")
	}
	var calls atomic.Int32
	req := execution.Request{Command: "sleep 10", Purpose: "停止生命周期验证", Profile: "local"}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := calls.Add(1)
		finish := "stop"
		delta := map[string]any{"content": "停止后总结"}
		if n == 1 {
			args, _ := json.Marshal(req)
			finish = "tool_calls"
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "sleep_call", "type": "function", "function": map[string]any{"name": "shell_exec", "arguments": string(args)}}}}
		}
		chunk, _ := json.Marshal(map[string]any{"id": "completion", "object": "chat.completion.chunk", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer provider.Close()
	dir := t.TempDir()
	cfg := &config.Config{DefaultProvider: "test", Providers: []config.Provider{{Name: "test", BaseURL: provider.URL + "/v1", APIKey: "test", Model: "m"}}, Memory: config.Memory{Core: config.MemoryCore{Dir: filepath.Join(dir, "memory")}}, Execution: execution.Config{Enabled: true, Backend: "os", Profiles: []execution.Profile{{Name: "local", Agents: []string{"root"}, Network: true, Rules: []execution.Rule{{Name: "test-sleep", Pattern: []string{"sleep"}, Decision: execution.Allow}}}}}}
	eng := engine.New(cfg)
	eng.SetStateRef(filepath.Join(dir, "state.db"))
	defer eng.Close()
	s := New(eng, nil)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- do(t, s, "POST", "/api/chat/stream", `{"message":"运行可取消命令"}`) }()
	session := ""
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.runs().mu.Lock()
		for id := range s.runs().running {
			session = strings.SplitN(id, "/", 2)[0]
			break
		}
		s.runs().mu.Unlock()
		if session != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if session == "" {
		t.Fatal("no running session")
	}
	time.Sleep(80 * time.Millisecond)
	stop := do(t, s, "POST", "/api/sessions/"+session+"/stop", `{}`)
	if stop.Code != 200 {
		t.Fatal(stop.Code, stop.Body.String())
	}
	select {
	case w := <-done:
		t.Logf("stream=%s", w.Body.String())
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not finish")
	}
	db, _ := eng.StateDB()
	var payload []byte
	if err := db.QueryRow(`SELECT payload FROM tool_results WHERE session_id=? AND call_id=?`, session, "sleep_call").Scan(&payload); err != nil {
		t.Fatal("cancelled result not durable", err)
	}
	var out execution.Observation
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	t.Logf("durable cancelled observation: executed=%t exit=%d error=%s", out.Executed, out.ExitCode, out.Error)
	if !out.Executed || !strings.Contains(out.Error, "取消") {
		t.Fatal("wrong cancellation observation", out)
	}
	var evidence string
	var ok bool
	if err := db.QueryRow(`SELECT evidence_id,ok FROM tool_calls WHERE session_id=? AND call_id=?`, session, "sleep_call").Scan(&evidence, &ok); err != nil {
		t.Fatal("cancellation audit missing", err)
	}
	if evidence == "" || ok {
		t.Fatal("cancellation audit must carry failed evidence", evidence, ok)
	}
	status, known := s.runs().sessionStatus(session)
	t.Logf("final status=%s known=%t", status, known)
	if status != TaskCancelled {
		t.Fatal("stopped handler did not report cancelled")
	}
}
