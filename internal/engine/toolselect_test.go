package engine

import (
	"context"
	"os/exec"
	"slices"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/gateway"
	jellymcp "github.com/jelly-agent/jelly-agent/internal/mcp"
	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/selector"
	"github.com/jelly-agent/jelly-agent/internal/toolreg"
)

// askingCtx is a ReadonlyContext carrying a question, which is the one thing
// selection reads out of it.
type askingCtx struct {
	agent.StrictContextMock
	question string
}

func (c *askingCtx) UserContent() *genai.Content {
	return &genai.Content{Parts: []*genai.Part{{Text: c.question}}}
}

// Selection has to work on the tools it will actually meet: MCP tools nobody
// described, whose only ranking signal is the name and description their own
// server advertised. A unit test over hand-built metadata cannot show that,
// because hand-built metadata has use cases and anti-examples that a real MCP
// server never sends.
func TestSelectionRanksRealMCPTools(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available for the MCP fixture server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reg, _ := toolreg.Build(nil)
	gw := gateway.New(gateway.Config{
		Registry: gateway.Snapshot(reg),
		Policy:   gateway.Policy{MaxSideEffect: ops.SideEffectMutating},
	})
	binder := &gateway.Binder{GW: gw, Registry: gateway.Snapshot(reg), Fallback: New(&config.Config{}).undeclaredFallback()}

	set, err := jellymcp.Toolset(ctx, config.MCPServer{
		Name: "k8s", Transport: "stdio", Command: "python3",
		Args: []string{"testdata/fake_mcp.py"},
		Env:  map[string]string{"FAKE_MCP_LABEL": "k8s"}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	sel := &selectingToolset{
		sets: []adktool.Toolset{binder.Toolset("k8s", set)},
		cfg:  selector.Config{MaxTools: 2},
	}

	names := func(question string) []string {
		t.Helper()
		got, err := sel.Tools(&askingCtx{agent.StrictContextMock{Ctx: ctx}, question})
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(got))
		for _, tl := range got {
			out = append(out, tl.Name())
		}
		return out
	}

	// The cap must actually bite: six tools, two slots.
	logs := names("看一下容器的 logs")
	if len(logs) != 2 {
		t.Fatalf("selected %v, want 2 of 6", logs)
	}
	if !slices.Contains(logs, "get_logs") {
		t.Errorf("selected %v, want get_logs for a question about logs", logs)
	}

	// A different question must reach a different tool, or the ranking is not
	// reading the question at all.
	sql := names("跑一条 mysql 查询")
	if !slices.Contains(sql, "query_mysql") {
		t.Errorf("selected %v, want query_mysql for a MySQL question", sql)
	}
	if slices.Equal(logs, sql) {
		t.Errorf("both questions selected %v — the question is not being read", logs)
	}
}

// Selection must be invisible to a deployment that has not asked for it. The
// built-ins are nine; the default budget must not touch them, or turning this
// on would change every existing install.
func TestDefaultBudgetDoesNotCutASmallCatalogue(t *testing.T) {
	tools := make([]adktool.Tool, 0, 9)
	for _, n := range []string{
		"web_search", "fetch_url", "remember", "forget",
		"load_memory", "use_skill", "run_script", "transfer_to_agent", "extra",
	} {
		tools = append(tools, &stubTool{name: n})
	}
	sel := &selectingToolset{static: tools, cfg: selector.Config{MaxTools: defaultMaxTools}}

	got, err := sel.Tools(&askingCtx{question: "任意问题"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(tools) {
		t.Errorf("selected %d of %d under the default budget — an existing install would change behaviour", len(got), len(tools))
	}
}

// A tool the gateway did not wrap has no metadata to rank on. It must still be
// ranked on what it does expose rather than sinking below everything by
// default.
func TestUnwrappedToolIsStillRankable(t *testing.T) {
	sel := &selectingToolset{
		static: []adktool.Tool{
			&stubTool{name: "noise_a"},
			&stubTool{name: "read_logs", desc: "读取容器日志"},
			&stubTool{name: "noise_b"},
		},
		cfg: selector.Config{MaxTools: 1},
	}
	got, err := sel.Tools(&askingCtx{question: "看下容器日志"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name() != "read_logs" {
		t.Errorf("selected %v, want read_logs", toolNames(got))
	}
}

type stubTool struct {
	name string
	desc string
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return s.desc }
func (s *stubTool) IsLongRunning() bool { return false }
func (s *stubTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: s.name, Description: s.desc}
}
func (s *stubTool) Run(agent.ToolContext, any) (map[string]any, error) { return nil, nil }

func toolNames(ts []adktool.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

// The result ceiling defaults to off, and that is safe only because history
// compaction is on. Turning both off leaves nothing bounding the context, and
// the symptom is a provider error that says nothing about tool results — so
// the condition has to be detectable and reported rather than discovered.
func TestContextGuardrails(t *testing.T) {
	zero := 0
	big := 24000
	cases := []struct {
		name      string
		history   *int
		resultCap int
		unguarded bool
	}{
		{"默认：压缩开、返回不限", nil, 0, false},
		{"压缩关、返回限了", &zero, 8000, false},
		{"压缩开、返回限了", &big, 8000, false},
		{"两者都关", &zero, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.History.MaxTokens = c.history
			cfg.Tools.MaxResultBytes = c.resultCap
			if got := New(cfg).contextUnguarded(); got != c.unguarded {
				t.Errorf("contextUnguarded() = %v, want %v", got, c.unguarded)
			}
		})
	}
}

// The default must be no ceiling. A ceiling that cuts a result the model
// cannot then use costs more than the bytes it saves: the run that produced
// this default retried seven times and spent 68k tokens to save two kilobytes.
func TestUndeclaredResultBoundDefaultsToUnlimited(t *testing.T) {
	if got := New(&config.Config{}).undeclaredFallback().MaxResultBytes; got != 0 {
		t.Errorf("default MaxResultBytes = %d, want 0 (no ceiling)", got)
	}
}

// And it must be settable, because a server that can return something
// genuinely enormous is a real case.
func TestUndeclaredResultBoundIsConfigurable(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.MaxResultBytes = 4096
	if got := New(cfg).undeclaredFallback().MaxResultBytes; got != 4096 {
		t.Errorf("MaxResultBytes = %d, want the configured 4096", got)
	}
}
