package engine

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
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
	h.fail("n9e-mcp", errors.New("i/o timeout"))
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

	h.fail("n9e-mcp", errors.New("i/o timeout"))
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

// The console has to be able to tell "down" from "we have not looked".
//
// A server nothing has consulted since the process started has reported
// nothing, and drawing that as healthy is a claim the console cannot make. It
// is exactly the case that made an unreachable host look like the agent simply
// being unable.
func TestHealthSnapshotSeparatesDownFromUnknown(t *testing.T) {
	h := newToolsetHealth()
	if got := h.snapshot(); len(got) != 0 {
		t.Errorf("a server nobody consulted appeared in the snapshot: %v", got)
	}

	h.fail("n9e-mcp", errors.New(`dial tcp 49.234.245.200:443: i/o timeout`))
	h.ok("k8s-mcp")

	snap := h.snapshot()
	down, ok := snap["n9e-mcp"]
	if !ok {
		t.Fatal("the failing server is missing from the snapshot")
	}
	if down.Up {
		t.Error("a server in cooldown is reported up")
	}
	// The reason has to survive: without it the console can say "不可用" but
	// not why, and the operator is back to reading process logs.
	if !strings.Contains(down.Error, "i/o timeout") {
		t.Errorf("error = %q, want the real reason", down.Error)
	}
	if down.RetryAt.IsZero() {
		t.Error("no retry time, so the console cannot say when it will be tried again")
	}

	up, ok := snap["k8s-mcp"]
	if !ok || !up.Up || up.Error != "" {
		t.Errorf("healthy server = %+v", up)
	}
	if up.CheckedAt.IsZero() {
		t.Error("a healthy server has no checked-at, so the console cannot say how fresh that is")
	}
}

// An expired cooldown means "try again", not "it is back".
//
// Clearing the failure on expiry made the console report 正常 for a server
// that had merely been down long enough — before anything had spoken to it. A
// liveness claim has to come from a live answer.
func TestAnExpiredCooldownDoesNotClaimRecovery(t *testing.T) {
	h := newToolsetHealth()
	h.fail("n9e-mcp", errors.New("i/o timeout"))

	// Expire it the way time would.
	h.mu.Lock()
	h.down["n9e-mcp"] = time.Now().Add(-time.Second)
	h.mu.Unlock()

	if h.skip("n9e-mcp") {
		t.Error("an expired cooldown still skips, so the server is never retried")
	}
	if snap := h.snapshot()["n9e-mcp"]; snap.Up {
		t.Error("a server reported up on an expired cooldown alone; nothing had spoken to it")
	}

	// Only a successful probe clears it.
	h.ok("n9e-mcp")
	if snap := h.snapshot()["n9e-mcp"]; !snap.Up || snap.Error != "" {
		t.Errorf("after a successful probe: %+v", snap)
	}
}

// Concurrent turns asking the same server for its tool list must cost one
// dial, not one each. The cooldown only helps once a failure is recorded;
// before that — the first turn after a host goes away, or several
// conversations in flight — every one of them waits on its own dial.
func TestConcurrentTurnsShareOneToolListing(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	l := newListing()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = l.do("n9e-mcp", func() ([]adktool.Tool, error) {
				calls.Add(1)
				<-release // hold every caller inside the one in-flight fetch
				return nil, errors.New("i/o timeout")
			})
		}()
	}
	// Let them pile up on the single call, then let it finish.
	for i := 0; i < 200 && calls.Load() == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("eight concurrent turns dialled %d times, want 1", n)
	}

	// And it is not a cache: the next round fetches again.
	_, _ = l.do("n9e-mcp", func() ([]adktool.Tool, error) { calls.Add(1); return nil, nil })
	if n := calls.Load(); n != 2 {
		t.Errorf("a later call reused a finished result (%d calls); the tool list must be re-read per turn", n)
	}
}

// blockingSet holds every caller inside one Tools() call, so concurrent turns
// can be observed piling up on the same fetch.
type blockingSet struct {
	calls   atomic.Int64
	release chan struct{}
}

func (b *blockingSet) Name() string { return "mcp_tool_set" }
func (b *blockingSet) Tools(agent.ReadonlyContext) ([]adktool.Tool, error) {
	b.calls.Add(1)
	<-b.release
	return nil, errors.New("i/o timeout")
}
func (b *blockingSet) Close() error { return nil }

// The collapsing has to be wired into the path that actually runs, not just
// available. Tested through selectingToolset.Tools because that is what a turn
// calls; a test of the helper alone stays green when the call site drops it.
func TestConcurrentTurnsShareOneDialThroughTheToolset(t *testing.T) {
	blocked := &blockingSet{release: make(chan struct{})}
	sel := &selectingToolset{
		static:   []adktool.Tool{&stubTool{name: "web_search"}},
		sets:     []namedSet{{name: "n9e-mcp", set: blocked}},
		health:   newToolsetHealth(),
		inflight: newListing(),
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sel.Tools(nil); err != nil {
				t.Errorf("a dead server aborted a turn: %v", err)
			}
		}()
	}
	for i := 0; i < 500 && blocked.calls.Load() == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	close(blocked.release)
	wg.Wait()

	if n := blocked.calls.Load(); n != 1 {
		t.Errorf("eight concurrent turns dialled %d times, want 1", n)
	}
}
