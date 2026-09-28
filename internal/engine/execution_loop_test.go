package engine

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

	"google.golang.org/adk/agent"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/execution"
	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

// Exercises the real ADK model/tool loop and OS sandbox against local fixtures:
// delegation -> help -> parameter failure -> corrected query -> evidence.
// The tool budget is deliberately too small to rely on fallback ranking.
func TestAssignedExecutorSurvivesTransferAndRepairsQuery(t *testing.T) {
	if !sandbox.OSSandboxAvailable() {
		t.Skip("OS sandbox required")
	}
	dir := t.TempDir()
	runtime := filepath.Join(dir, "runtime")
	os.MkdirAll(filepath.Join(runtime, "bin"), 0700)
	cli := `#!/bin/sh
if [ "$TOKEN" != "scoped-test-identity" ]; then exit 9; fi
if [ "$3" = "help" ]; then printf 'DescribeTopics options: --Offset --Limit'; exit 0; fi
if [ "$3" = "--limit" ]; then printf 'Unknown option --limit; use --Limit' >&2; exit 255; fi
printf '{"Response":{"Topics":[{"TopicId":"topic-demo"}],"TotalCount":1,"RequestId":"request-demo"}}'
`
	if err := os.WriteFile(filepath.Join(runtime, "bin", "tccli"), []byte(cli), 0700); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		n := calls.Add(1)
		if strings.Contains(string(b), "scoped-test-identity") {
			t.Error("credential sent to model")
		}
		var request struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		json.Unmarshal(b, &request)
		hasExec := false
		for _, tl := range request.Tools {
			if tl.Function.Name == "shell_exec" {
				hasExec = true
			}
		}
		if (n == 1 && hasExec) || (n > 1 && !hasExec) {
			t.Errorf("executor wrong scope or pruned at step %d", n)
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			io.WriteString(w, toolCallCompletion("transfer_to_agent", `{\"agent_name\":\"TencentQuery\"}`))
			return
		}
		commands := map[int32]string{2: "tccli cls DescribeTopics help", 3: "tccli cls DescribeTopics --limit 100", 4: "tccli cls DescribeTopics --Limit 100"}
		if command, ok := commands[n]; ok {
			if n == 3 && !strings.Contains(string(b), "--Offset --Limit") {
				t.Error("help not returned to model")
			}
			if n == 4 && !strings.Contains(string(b), "Unknown option --limit") {
				t.Error("failure not returned to model")
			}
			args, _ := json.Marshal(execution.Request{Command: command, Purpose: "query CLS", Profile: "cloud"})
			escaped, _ := json.Marshal(string(args))
			io.WriteString(w, toolCallCompletion("shell_exec", string(escaped[1:len(escaped)-1])))
			return
		}
		if n != 5 || !strings.Contains(string(b), "topic-demo") || !strings.Contains(string(b), "evidence_id") {
			t.Errorf("loop did not reach query evidence at step %d", n)
		}
		io.WriteString(w, `{"id":"final","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"found topic-demo"},"finish_reason":"stop"}]}`)
	}))
	defer provider.Close()
	none := []string{}
	cfg := &config.Config{SourcePath: filepath.Join(dir, "config.yaml"), DefaultProvider: "test", Providers: []config.Provider{{Name: "test", BaseURL: provider.URL, APIKey: "test", Model: "m", ContextWindow: 100000, MaxTokens: 1024}}, Memory: config.Memory{Core: config.MemoryCore{Dir: filepath.Join(dir, "memory")}}, Tools: config.Tools{MaxTools: 1}, Agents: []config.AgentDef{
		{Name: "OrchestrationAgent", Enabled: true, SubAgents: []string{"TencentQuery"}, Skills: &none},
		{Name: "TencentQuery", Enabled: true, Skills: &none},
	}, AgentVars: map[string]map[string]string{"TencentQuery": {"CLOUD_KEY": "scoped-test-identity"}}, Execution: execution.Config{Enabled: true, Backend: "os", Profiles: []execution.Profile{{Name: "cloud", Agents: []string{"TencentQuery"}, ToolDir: runtime, AgentEnv: map[string]string{"TOKEN": "CLOUD_KEY"}}}}}
	e := New(cfg)
	e.SetStateRef(filepath.Join(dir, "state.db"))
	t.Cleanup(e.Close)
	a, _, _, search, err := e.BuildAgentByName("OrchestrationAgent")
	if err != nil {
		t.Fatal(err)
	}
	if search != nil {
		defer search.Close()
	}
	run, svc, err := e.NewRunner(a, search)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(t.Context(), &adksession.CreateRequest{AppName: AppName, UserID: UserID, SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	for _, err := range run.Run(t.Context(), UserID, "s", genai.NewContentFromText("帮我查腾讯云日志主题", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 5 {
		t.Fatal(fmt.Sprintf("expected complete 5-step loop, got %d", calls.Load()))
	}
	if len(cfg.Agents[1].RequiredTools) != 0 {
		t.Fatal("automatic pin mutated user's config")
	}
}
