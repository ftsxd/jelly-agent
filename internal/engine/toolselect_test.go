package engine

import (
	"context"
	"os/exec"
	"slices"
	"strings"
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
	session  string
}

func (c *askingCtx) SessionID() string { return c.session }

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
		got, err := sel.Tools(&askingCtx{StrictContextMock: agent.StrictContextMock{Ctx: ctx}, question: question})
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
	// meta, when set, is what selection ranks against — the same path a
	// gateway-wrapped tool takes. Without it a stub can only carry a name and
	// a description, which is not enough to exercise fields like latency.
	meta *ops.ToolMetadata
}

func (s *stubTool) Metadata() ops.ToolMetadata {
	if s.meta != nil {
		return *s.meta
	}
	return ops.ToolMetadata{Name: s.name, Description: s.desc}
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

// The whole point of the admission record, at the level it is wired in.
//
// Four tools and three slots, with the padding declared first so that a fresh
// selection would spend both spare slots on it. Turn one matches the log tool,
// turn two matches the SQL one; without the standing set, turn two drops the
// log tool and the prompt prefix changes, taking the cache for the whole
// history with it.
//
// An earlier version of this test used three tools and two slots, where both
// turns happened to select the same pair — it passed with the admission record
// removed entirely.
func TestSecondTurnKeepsTheSameToolSet(t *testing.T) {
	tools := []adktool.Tool{
		&stubTool{name: "pad_one"},
		&stubTool{name: "pad_two"},
		&stubTool{name: "get_logs", desc: "读取容器日志"},
		&stubTool{name: "query_mysql", desc: "执行 MySQL 查询"},
	}
	sel := &selectingToolset{
		static: tools,
		cfg:    selector.Config{MaxTools: 3},
		admit:  newAdmissions(),
	}

	ask := func(q string) []string {
		t.Helper()
		got, err := sel.Tools(&askingCtx{question: q, session: "s1"})
		if err != nil {
			t.Fatal(err)
		}
		return toolNames(got)
	}

	first := ask("看下容器日志")
	if !slices.Contains(first, "get_logs") {
		t.Fatalf("first turn = %v, want get_logs", first)
	}

	second := ask("跑一条 mysql 查询")
	if !slices.Contains(second, "query_mysql") {
		t.Fatalf("second turn = %v, want the newly matched tool", second)
	}
	if !slices.Contains(second, "get_logs") {
		t.Errorf("second turn = %v, want the first turn's tool kept over padding", second)
	}
	if len(second) > 3 {
		t.Errorf("second turn = %v, over the budget of 3", second)
	}

	if third := ask("看下容器日志"); !slices.Equal(third, second) {
		t.Errorf("third turn = %v, want it identical to %v — the set has settled", third, second)
	}
}

// Separate conversations must not inherit each other's tools.
func TestDifferentSessionsDoNotShareTools(t *testing.T) {
	tools := []adktool.Tool{
		&stubTool{name: "get_logs", desc: "读取容器日志"},
		&stubTool{name: "query_mysql", desc: "执行 MySQL 查询"},
	}
	sel := &selectingToolset{static: tools, cfg: selector.Config{MaxTools: 1}, admit: newAdmissions()}

	a, err := sel.Tools(&askingCtx{question: "看下容器日志", session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := sel.Tools(&askingCtx{question: "跑一条 mysql 查询", session: "s2"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Equal(toolNames(a), toolNames(b)) {
		t.Errorf("both sessions got %v; each should get its own", toolNames(a))
	}
}

// The history budget is derived from what the provider says its model accepts,
// because it cannot be discovered: OpenAI-compatible endpoints do not report
// it, and a model-name table inside the binary goes stale.
func TestHistoryBudgetFollowsTheContextWindow(t *testing.T) {
	e := New(&config.Config{})
	if got := e.historyBudgetFor(64000); got != 38400 {
		t.Errorf("budget for a 64k window = %d, want 38400 (60%%)", got)
	}
	// Unknown window falls back rather than guessing.
	if got := e.historyBudgetFor(0); got != 0 {
		t.Errorf("budget for an unknown window = %d, want 0 so the package default applies", got)
	}
	// An explicit setting always wins — it is the operator's number.
	n := 12345
	cfg := &config.Config{}
	cfg.History.MaxTokens = &n
	if got := New(cfg).historyBudgetFor(1000000); got != 12345 {
		t.Errorf("budget = %d, want the configured 12345", got)
	}
}

// A slot is earned by matching the question, not by being quick.
//
// The score carries a latency tiebreaker, so a fast tool that matched nothing
// still scores above zero. Treating that as a match pins padding in place and
// displaces the tool a previous turn actually used — the prefix changes, and
// the history behind it goes uncached.
//
// Two slots. Turn one matches the log tool and pads with the fast one; turn
// two matches SQL. Correct behaviour keeps the log tool and drops the padding;
// reading "matched" off the score keeps the padding and drops the log tool.
func TestAFastToolDoesNotEarnASlot(t *testing.T) {
	fast := ops.ToolMetadata{Name: "quick_pad", Latency: ops.LatencyFast}
	logs := ops.ToolMetadata{Name: "get_logs", UseCases: []string{"查看容器日志"}}
	sql := ops.ToolMetadata{Name: "query_mysql", UseCases: []string{"执行 MySQL 查询"}}
	sel := &selectingToolset{
		static: []adktool.Tool{
			&stubTool{name: fast.Name, meta: &fast},
			&stubTool{name: logs.Name, meta: &logs},
			&stubTool{name: sql.Name, meta: &sql},
		},
		cfg:   selector.Config{MaxTools: 2},
		admit: newAdmissions(),
	}
	ask := func(q string) []string {
		t.Helper()
		got, err := sel.Tools(&askingCtx{question: q, session: "s1"})
		if err != nil {
			t.Fatal(err)
		}
		return toolNames(got)
	}

	first := ask("查看容器日志")
	if !slices.Contains(first, "get_logs") {
		t.Fatalf("first turn = %v, want get_logs", first)
	}
	second := ask("执行 MySQL 查询")
	if !slices.Contains(second, "query_mysql") {
		t.Fatalf("second turn = %v, want query_mysql", second)
	}
	if !slices.Contains(second, "get_logs") {
		t.Errorf("second turn = %v, want the previous turn's tool kept over the fast padding", second)
	}
}

// The record has to name the set the request carries.
//
// Admission keeps tools the ranking cut and drops filler the ranking kept, so
// a log written before that step describes a different request than the one
// that went out. This log exists to explain why a tool was or was not offered,
// and it is the evidence any cache-hit analysis rests on — disagreeing with
// the request is the one thing it must never do.
func TestSelectionLogNamesWhatWasSent(t *testing.T) {
	pad := ops.ToolMetadata{Name: "pad"}
	logs := ops.ToolMetadata{Name: "get_logs", UseCases: []string{"查看容器日志"}}
	adm := newAdmissions()
	adm.admit("s1", pick{matched: []string{"get_logs"}}, map[string]int{"get_logs": 1}, 1)

	var logged selector.Result
	sel := &selectingToolset{
		static: []adktool.Tool{
			&stubTool{name: pad.Name, meta: &pad},
			&stubTool{name: logs.Name, meta: &logs},
		},
		cfg:    selector.Config{MaxTools: 1},
		admit:  adm,
		report: func(r selector.Result) { logged = r },
	}
	got, err := sel.Tools(&askingCtx{question: "毫不相关的问题", session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	sent := toolNames(got)
	if !slices.Equal(logged.Selected, sent) {
		t.Fatalf("log says %v, request carried %v", logged.Selected, sent)
	}

	// And it says why each tool ended up where it did, or the record explains
	// nothing.
	for _, c := range logged.Candidates {
		inSet := slices.Contains(sent, c.Tool)
		if inSet && c.Suppressed != "" {
			t.Errorf("%s was sent but is marked suppressed (%q)", c.Tool, c.Suppressed)
		}
		if !inSet && c.Suppressed == "" {
			t.Errorf("%s was not sent but carries no reason", c.Tool)
		}
	}
}

// A tool the ranking cut but admission kept must say so, not carry a stale
// "over budget" note.
func TestRetainedToolExplainsItself(t *testing.T) {
	pad := ops.ToolMetadata{Name: "pad"}
	logs := ops.ToolMetadata{Name: "get_logs", UseCases: []string{"查看容器日志"}}
	adm := newAdmissions()
	adm.admit("s1", pick{matched: []string{"get_logs"}}, map[string]int{"get_logs": 1}, 1)

	var logged selector.Result
	sel := &selectingToolset{
		static: []adktool.Tool{
			&stubTool{name: pad.Name, meta: &pad},
			&stubTool{name: logs.Name, meta: &logs},
		},
		cfg:    selector.Config{MaxTools: 1},
		admit:  adm,
		report: func(r selector.Result) { logged = r },
	}
	if _, err := sel.Tools(&askingCtx{question: "毫不相关的问题", session: "s1"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range logged.Candidates {
		if c.Tool == "get_logs" && !strings.Contains(c.Reason, "缓存") {
			t.Errorf("get_logs reason = %q, want it to say it was kept for the cache prefix", c.Reason)
		}
	}
}
