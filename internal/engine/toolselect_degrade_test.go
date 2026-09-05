package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"
)

// deadSet is an MCP server that will not answer — the shape of a host that has
// gone away, which fails on dial rather than on the protocol.
type deadSet struct{ calls int }

func (d *deadSet) Name() string { return "mcp_tool_set" }
func (d *deadSet) Tools(agent.ReadonlyContext) ([]adktool.Tool, error) {
	d.calls++
	return nil, errors.New(`failed to init MCP session: calling "initialize": ` +
		`dial tcp 49.234.245.200:443: i/o timeout`)
}
func (d *deadSet) Close() error { return nil }

// An unreachable MCP server must not take the conversation with it.
//
// The observed failure: n9e stopped answering, Tools returned the dial error,
// ADK aborted the invocation on it, and "你好" could not be answered either.
// The tools that server offers are unavailable; talking is not.
func TestADeadMCPServerDoesNotBlockTheTurn(t *testing.T) {
	sel := &selectingToolset{
		static: []adktool.Tool{&stubTool{name: "web_search"}, &stubTool{name: "remember"}},
		sets:   []namedSet{{name: "n9e-mcp", set: &deadSet{}}},
		health: newToolsetHealth(),
	}

	got, err := sel.Tools(nil)
	if err != nil {
		t.Fatalf("a dead MCP server aborted the turn: %v", err)
	}
	names := make([]string, 0, len(got))
	for _, tool := range got {
		names = append(names, tool.Name())
	}
	if len(names) == 0 {
		t.Fatal("no tools at all; the built-ins do not depend on that server")
	}
	if !containsName(names, "web_search") {
		t.Errorf("built-in tools = %v, want the ones that never needed the dead server", names)
	}
}

// And it must not cost a dial timeout on every turn. The tool list is fetched
// once per user message, so a thirty-second dial made every conversation
// thirty seconds slower — including the ones that would never have called it.
func TestADeadMCPServerIsNotRetriedEveryTurn(t *testing.T) {
	dead := &deadSet{}
	sel := &selectingToolset{
		static: []adktool.Tool{&stubTool{name: "web_search"}},
		sets:   []namedSet{{name: "n9e-mcp", set: dead}},
		health: newToolsetHealth(),
	}
	for i := 0; i < 5; i++ {
		if _, err := sel.Tools(nil); err != nil {
			t.Fatal(err)
		}
	}
	if dead.calls != 1 {
		t.Errorf("the dead server was dialled %d times across five turns; the cooldown did not hold", dead.calls)
	}
}

// The cooldown is a liveness cache, not a circuit breaker: a server that comes
// back has to be picked up again, not kept out.
func TestARecoveredMCPServerComesBack(t *testing.T) {
	h := newToolsetHealth()
	h.fail("n9e-mcp")
	if !h.skip("n9e-mcp") {
		t.Fatal("a just-failed server was not in cooldown")
	}
	// Expire it the way time would.
	h.mu.Lock()
	h.down["n9e-mcp"] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if h.skip("n9e-mcp") {
		t.Error("the cooldown never expires, so a recovered server stays out")
	}

	h.fail("n9e-mcp")
	h.ok("n9e-mcp")
	if h.skip("n9e-mcp") {
		t.Error("a server that answered is still marked down")
	}
}

// One server failing must not hide another that is fine.
func TestOnlyTheFailingServerIsSkipped(t *testing.T) {
	sel := &selectingToolset{
		static: []adktool.Tool{&stubTool{name: "web_search"}},
		sets: []namedSet{
			{name: "n9e-mcp", set: &deadSet{}},
			{name: "k8s-mcp", set: &staticSet{tools: []adktool.Tool{&stubTool{name: "get_pods"}}}},
		},
		health: newToolsetHealth(),
	}
	got, err := sel.Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(got))
	for _, tool := range got {
		names = append(names, tool.Name())
	}
	if !containsName(names, "get_pods") {
		t.Errorf("tools = %v, want the healthy server's tools alongside the built-ins", names)
	}
}

type staticSet struct{ tools []adktool.Tool }

func (s *staticSet) Name() string                                        { return "mcp_tool_set" }
func (s *staticSet) Tools(agent.ReadonlyContext) ([]adktool.Tool, error) { return s.tools, nil }
func (s *staticSet) Close() error                                        { return nil }

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want || strings.HasSuffix(n, "__"+want) {
			return true
		}
	}
	return false
}
