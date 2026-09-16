package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/codeproject"
	"github.com/jelly-agent/jelly-agent/internal/config"
)

// A project is assigned to the sub-agent, and the question arrives at the
// coordinator. What the sub-agent's tools see after a transfer is the whole
// question this test exists for: the identity is captured when the node is
// built, and a coordinator that hands its own identity down would present as
// "授权了但看不到" — with a transcript that names the sub-agent either way.
func TestTransferredSubAgentSeesItsOwnProjectGrant(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "code-projects", "snapshot-x")
	if err := os.MkdirAll(filepath.Join(snapshot, "services"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "services", "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	b, _ := json.Marshal([]codeproject.Project{{
		ID: "Silkworm", Name: "Silkworm", URL: "https://git.example.com/silkworm.git",
		Branch: "master", RootPath: ".", Snapshot: "snapshot-x", SyncedAt: &now,
		Grants: []codeproject.Grant{{Agent: "CodeAnalyzer"}},
	}})
	if err := os.WriteFile(filepath.Join(dir, "code-projects", "projects.json"), b, 0600); err != nil {
		t.Fatal(err)
	}

	// The model is scripted, not simulated: transfer, then list, then answer.
	// The third request carries the tool result, which is the observation.
	var mu sync.Mutex
	var toolResult string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls++
		n := calls
		if strings.Contains(string(body), `"role":"tool"`) {
			toolResult = string(body)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case n == 1:
			io.WriteString(w, toolCallCompletion("transfer_to_agent", `{\"agent_name\":\"CodeAnalyzer\"}`))
		case !strings.Contains(string(body), `"role":"tool"`):
			io.WriteString(w, toolCallCompletion("list_code_projects", `{}`))
		default:
			io.WriteString(w, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	e := New(&config.Config{
		SourcePath: filepath.Join(dir, "config.yaml"),
		// Core memory is pointed at the temp directory for the same reason the
		// state database is: the default is ~/.jelly-agent/memory, and a test
		// that reads it renders whatever this developer's agent happens to
		// remember into the prompt it asserts on.
		Memory:          config.Memory{Core: config.MemoryCore{Dir: filepath.Join(dir, "memory")}},
		DefaultProvider: "test",
		Providers: []config.Provider{{
			Name: "test", BaseURL: srv.URL, APIKey: "k", Model: "m",
			ContextWindow: 100000, MaxTokens: 1024,
		}},
		Agents: []config.AgentDef{
			{Name: "OrchestrationAgent", Enabled: true, SubAgents: []string{"CodeAnalyzer"}, Instruction: "你是协调者。"},
			{Name: "CodeAnalyzer", Enabled: true, Instruction: "你负责代码分析。"},
		},
	})
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
	ctx := t.Context()
	if _, err := svc.Create(ctx, &adksession.CreateRequest{AppName: AppName, UserID: UserID, SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	msg := genai.NewContentFromText("silkworm 里有哪些服务？", genai.RoleUser)
	for _, err := range run.Run(ctx, UserID, "s1", msg, agent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	got := toolResult
	mu.Unlock()
	if got == "" {
		t.Fatal("模型从未收到工具结果，脚本没跑到那一步")
	}

	// Asserted on the decoded tool message rather than on the raw request: the
	// request also carries the system instruction and every tool schema, and a
	// substring match over all of that can pass on a mention of the project in
	// text that has nothing to do with what the tool returned.
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(got), &req); err != nil {
		t.Fatalf("解析发给模型的请求: %v", err)
	}
	var content string
	for _, m := range req.Messages {
		if m.Role == "tool" {
			content = m.Content
		}
	}
	if content == "" {
		t.Fatalf("请求里没有工具结果消息：%s", got)
	}
	var payload struct {
		Data struct {
			Agent    string `json:"agent"`
			Hint     string `json:"hint"`
			Projects []struct {
				ID string `json:"id"`
			} `json:"projects"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatalf("解析工具结果 %q: %v", content, err)
	}
	if payload.Data.Agent != "CodeAnalyzer" {
		t.Errorf("工具以 %q 的身份执行，应为 CodeAnalyzer：%s", payload.Data.Agent, content)
	}
	if len(payload.Data.Projects) != 1 || payload.Data.Projects[0].ID != "Silkworm" {
		t.Errorf("转交过去的子 agent 没看到分配给它的项目：%s", content)
	}
	if payload.Data.Hint != "" {
		t.Errorf("列表非空却带了未分配提示：%s", content)
	}
}

func toolCallCompletion(name, args string) string {
	return `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"` +
		name + `","arguments":"` + args + `"}}]},"finish_reason":"tool_calls"}]}`
}
