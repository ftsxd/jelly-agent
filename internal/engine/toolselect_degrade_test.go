package engine

import (
	"context"
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
	started := make(chan struct{})
	l := newListing()

	fetch := func(context.Context) ([]adktool.Tool, error) {
		calls.Add(1)
		close(started)
		<-release // hold the fetch open while the others arrive
		return nil, errors.New("i/o timeout")
	}
	leader := make(chan struct{})
	go func() { defer close(leader); _, _ = l.do(context.Background(), "n9e-mcp", fetch) }()
	<-started // the shared call is registered and running

	// The others arrive while the fetch is still held open, and the fetch is
	// released only once they are all inside it.
	//
	// The old version released as soon as the first dial was counted, which is
	// before the followers necessarily got there: one that arrived after the
	// shared call had finished dialled again — legitimately, since the call
	// was over — and the count read that as the collapsing having failed. The
	// test was asserting a timing coincidence.
	var arrived, wg sync.WaitGroup
	for range 8 {
		arrived.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			arrived.Done()
			_, _ = l.do(context.Background(), "n9e-mcp", func(context.Context) ([]adktool.Tool, error) {
				calls.Add(1)
				return nil, errors.New("a follower dialled on its own")
			})
		}()
	}
	arrived.Wait()
	// Nothing can finish while release is open, so this only has to be long
	// enough for eight goroutines to reach an uncontended map lookup.
	time.Sleep(50 * time.Millisecond)

	close(release)
	wg.Wait()
	<-leader

	if n := calls.Load(); n != 1 {
		t.Errorf("eight concurrent turns dialled %d times, want 1", n)
	}
}

// And it is not a cache: once the shared call is over, the next round dials
// again, because the tool list is meant to be re-read per turn.
func TestAFinishedListingIsNotReused(t *testing.T) {
	var calls atomic.Int64
	l := newListing()
	fetch := func(context.Context) ([]adktool.Tool, error) {
		calls.Add(1)
		return nil, nil
	}
	if _, err := l.do(context.Background(), "n9e-mcp", fetch); err != nil {
		t.Fatal(err)
	}
	// The slot is freed on the fetch's own goroutine, just after the answer is
	// published, so a caller can return before it is gone.
	waitForNoInFlight(t, l, "n9e-mcp")

	if _, err := l.do(context.Background(), "n9e-mcp", fetch); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("a later call reused a finished result (%d calls); the tool list must be re-read per turn", n)
	}
}

func waitForNoInFlight(t *testing.T, l *listing, name string) {
	t.Helper()
	for i := 0; i < 2000; i++ {
		l.mu.Lock()
		_, present := l.calls[name]
		l.mu.Unlock()
		if !present {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("in-flight call was never cleaned up")
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

// A caller that gives up must not take the shared fetch down with it.
//
// The first version handed the leader's context to the merged call, so when
// the leader was cancelled the survivors inherited "context canceled", saw
// only the built-in tools, and marked a perfectly healthy server down for a
// minute — because of a conversation that had nothing to do with it.
func TestACancellingCallerDoesNotPoisonTheOthers(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var sawCancel atomic.Bool

	l := newListing()
	fetch := func(run context.Context) ([]adktool.Tool, error) {
		close(started)
		<-release
		if run.Err() != nil {
			sawCancel.Store(true)
			return nil, run.Err()
		}
		return []adktool.Tool{&stubTool{name: "get_pods"}}, nil
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	// The leader's own outcome is not the subject here — it left, so it gets
	// its own cancellation. What matters is what the survivor gets.
	leaderDone := make(chan struct{})
	go func() { defer close(leaderDone); _, _ = l.do(leaderCtx, "k8s-mcp", fetch) }()
	<-started

	var wg sync.WaitGroup
	var survivorTools []adktool.Tool
	var survivorErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		survivorTools, survivorErr = l.do(context.Background(), "k8s-mcp", fetch)
	}()

	cancelLeader() // the leader walks away
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if sawCancel.Load() {
		t.Error("the merged fetch inherited the leader's cancellation")
	}
	if survivorErr != nil {
		t.Errorf("a survivor got %v; the server was fine, another caller merely left", survivorErr)
	}
	if len(survivorTools) != 1 {
		t.Errorf("survivor got %d tools, want the server's answer", len(survivorTools))
	}
	<-leaderDone
}

// And a waiter must be able to leave on its own context rather than being
// parked on a channel with nothing else to select on.
func TestAWaiterLeavesOnItsOwnCancellation(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})

	l := newListing()
	go func() {
		_, _ = l.do(context.Background(), "k8s-mcp", func(context.Context) ([]adktool.Tool, error) {
			close(started)
			<-release
			return nil, nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := l.do(ctx, "k8s-mcp", func(context.Context) ([]adktool.Tool, error) { return nil, nil })
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiter returned %v, want its own context error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled waiter stayed parked on the shared fetch")
	}
}

// A caller's own cancellation must not be recorded as a server fault.
func TestACancelledTurnDoesNotMarkTheServerDown(t *testing.T) {
	blocked := &blockingSet{release: make(chan struct{})}
	defer close(blocked.release)
	h := newToolsetHealth()
	sel := &selectingToolset{
		static:   []adktool.Tool{&stubTool{name: "web_search"}},
		sets:     []namedSet{{name: "k8s-mcp", set: blocked}},
		health:   h,
		inflight: newListing(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sel.Tools(&askingCtx{StrictContextMock: agent.StrictContextMock{Ctx: ctx}})
	}()
	for i := 0; i < 500 && blocked.calls.Load() == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if snap, ok := h.snapshot()["k8s-mcp"]; ok && !snap.Up {
		t.Errorf("an abandoned turn marked the server down: %+v", snap)
	}
}

// Discovery has to have a deadline of its own.
//
// Dropping the transport's request timeout was right for tool execution — a
// wide log query legitimately takes a minute — but nothing then bounded the
// handshake and the list that precede any call. A host that connects,
// completes TLS and never answers left the turn hanging with no deadline at
// all, and every merged waiter behind it.
func TestDiscoveryHasItsOwnDeadline(t *testing.T) {
	l := newListing()
	var deadline time.Duration
	_, err := l.do(context.Background(), "mute-mcp", func(run context.Context) ([]adktool.Tool, error) {
		d, ok := run.Deadline()
		if !ok {
			return nil, errors.New("no deadline on the discovery context")
		}
		deadline = time.Until(d)
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if deadline <= 0 || deadline > discoverTimeout+time.Second {
		t.Errorf("discovery deadline = %v, want about %v", deadline, discoverTimeout)
	}
}
