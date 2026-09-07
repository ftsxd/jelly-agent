package engine

// Remembering which MCP servers are not answering.
//
// Two problems, one cause. A server that has gone away costs a dial timeout —
// thirty seconds by default — and it costs it on every turn, because the tool
// list is fetched per user message. So a single unreachable server makes every
// conversation thirty seconds slower, including the ones that would never have
// called it.
//
// The cooldown is short on purpose. This is a liveness cache, not a circuit
// breaker: the point is to stop paying the timeout repeatedly while a server
// is down, not to keep it out once it comes back. A minute means a recovered
// server is picked up on the next turn or two, and a dead one is skipped
// instantly in between.

import (
	"context"
	"sync"
	"time"

	"google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"
)

// toolsetCooldown is how long a failed server is skipped before being tried
// again.
const toolsetCooldown = time.Minute

// ServerHealth is what is known about one MCP server's liveness.
//
// The error text is kept, not just the fact of failure. A silent degradation
// is the thing to avoid here: with the server skipped the model answers as if
// those tools do not exist, and "我查不到告警" reads to the user as the agent
// being unable rather than as a host being unreachable. Somebody has to be
// able to see which it was, and the reason has to be the real one.
type ServerHealth struct {
	Name     string    `json:"name"`
	Up       bool      `json:"up"`
	Error    string    `json:"error,omitempty"`
	FailedAt time.Time `json:"failed_at,omitzero"`
	RetryAt  time.Time `json:"retry_at,omitzero"`
	// CheckedAt is when this server last answered or failed. Zero means it has
	// not been consulted yet this process — which is not the same as healthy,
	// and the console must not draw it as such.
	CheckedAt time.Time `json:"checked_at,omitzero"`
}

// toolsetHealth records which servers recently failed to answer.
type toolsetHealth struct {
	mu   sync.Mutex
	down map[string]time.Time // server name → when to try again
	last map[string]error     // server name → the failure, for the console
	seen map[string]time.Time // server name → when it was last consulted
}

func newToolsetHealth() *toolsetHealth {
	return &toolsetHealth{
		down: map[string]time.Time{},
		last: map[string]error{},
		seen: map[string]time.Time{},
	}
}

// snapshot reports what is known about every server consulted so far.
func (h *toolsetHealth) snapshot() map[string]ServerHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]ServerHealth, len(h.seen))
	for name, at := range h.seen {
		e := ServerHealth{Name: name, Up: true, CheckedAt: at}
		if until, down := h.down[name]; down {
			// Still down until a probe says otherwise, whether or not the
			// cooldown has run out. A past retry time reads as "due to be
			// retried", which is exactly the state.
			e.Up, e.RetryAt, e.FailedAt = false, until, at
			if err := h.last[name]; err != nil {
				e.Error = err.Error()
			}
		}
		out[name] = e
	}
	return out
}

// skip reports whether this server is still in its cooldown.
//
// An expired cooldown means "try again", not "it is back". The failure record
// stays until a probe actually succeeds — clearing it here made the console
// report 正常 for a server that had merely been down long enough, before
// anything had spoken to it. A liveness claim has to come from a live answer.
func (h *toolsetHealth) skip(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	until, ok := h.down[name]
	return ok && time.Now().Before(until)
}

// fail starts (or extends) a server's cooldown, keeping why.
func (h *toolsetHealth) fail(name string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.down[name] = time.Now().Add(toolsetCooldown)
	h.last[name] = err
	h.seen[name] = time.Now()
}

// ok clears a server's cooldown.
func (h *toolsetHealth) ok(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.down, name)
	delete(h.last, name)
	h.seen[name] = time.Now()
}

// discoverTimeout bounds listing a server's tools.
//
// Removing the transport's request deadline was right for tool execution — a
// log query over a wide window legitimately takes a minute — but discovery is
// not execution. The gateway's per-tool timeout only covers a call; nothing
// covered the handshake and the list that precede it. A host that connects,
// completes TLS and then simply never answers left the turn, and every merged
// waiter behind it, hanging indefinitely.
//
// Twenty seconds is far more than a handshake and a list should need, and far
// less than a conversation should wait to find out a server is mute.
const discoverTimeout = 20 * time.Second

// listing collapses concurrent tool-list fetches for one server.
//
// The cooldown only helps once a failure has been recorded. Before that — the
// first turn after a host goes away, or several conversations in flight at
// once — every one of them dials it in parallel and every one waits. They are
// asking the same question of the same server at the same moment, so one
// answer serves all of them.
//
// Two things the first version got wrong, both from sharing the leader's
// context with everyone behind it. A caller that gave up took the shared
// fetch down with it: the survivors inherited "context canceled", saw only
// the built-in tools, and marked a perfectly healthy server down for a
// minute. And a waiter could not leave on its own cancellation, because it
// was parked on a channel with nothing else to select on.
//
// So the merged fetch runs on its own lifetime — the caller's values, none of
// the caller's cancellation, and a deadline of its own — and each waiter
// leaves when its own context says to.
//
// Deliberately not a cache: the result is shared only for the duration of one
// in-flight call. Holding it any longer would freeze a tool list that is meant
// to be re-read per turn.
type listing struct {
	mu    sync.Mutex
	calls map[string]*listingCall
}

type listingCall struct {
	done  chan struct{}
	tools []adktool.Tool
	err   error
}

func newListing() *listing { return &listing{calls: map[string]*listingCall{}} }

// do runs fn for name, or joins an in-flight run of it.
//
// fn is handed a context that outlives whichever caller happened to start it,
// so one caller walking away does not fail the rest.
func (l *listing) do(ctx context.Context, name string, fn func(context.Context) ([]adktool.Tool, error)) ([]adktool.Tool, error) {
	l.mu.Lock()
	if c, running := l.calls[name]; running {
		l.mu.Unlock()
		return c.wait(ctx)
	}
	c := &listingCall{done: make(chan struct{})}
	l.calls[name] = c
	l.mu.Unlock()

	go func() {
		// Deferred in this order because defers run last-registered-first: the
		// answer is published, and only then is the slot freed.
		//
		// The other order left a window between the two — the entry already
		// gone, the result not yet readable — and a caller arriving inside it
		// found no in-flight call, no answer, and dialled the server again for
		// a result that was sitting a microsecond away. It is a small window
		// and it hit two or three times in twenty turns, which is exactly the
		// rate at which it looks like something else.
		defer func() {
			l.mu.Lock()
			delete(l.calls, name)
			l.mu.Unlock()
		}()
		defer close(c.done)

		// Values carried, cancellation dropped, deadline ours. A leader that
		// goes away must not cancel the answer everyone else is waiting for.
		base := context.WithoutCancel(orBackground(ctx))
		run, cancel := context.WithTimeout(base, discoverTimeout)
		defer cancel()

		c.tools, c.err = fn(run)
	}()

	return c.wait(ctx)
}

// wait blocks for the shared answer, or for this caller's own context.
func (c *listingCall) wait(ctx context.Context) ([]adktool.Tool, error) {
	if ctx == nil {
		<-c.done
		return c.tools, c.err
	}
	select {
	case <-c.done:
		return c.tools, c.err
	case <-ctx.Done():
		// This caller is leaving; the fetch continues for the others. The
		// error is the caller's own, which is how the call site knows not to
		// blame the server for it.
		return nil, ctx.Err()
	}
}

func orBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// namedSet pairs a bound toolset with the server it came from, so a failure
// can say which server failed. ADK's Toolset interface cannot: mcptoolset
// reports the constant "mcp_tool_set" for every instance.
type namedSet struct {
	name string
	set  adktool.Toolset
}

func (n namedSet) tools(ctx agent.ReadonlyContext) ([]adktool.Tool, error) {
	return n.set.Tools(ctx)
}

// withDeadline substitutes a context's lifetime while keeping everything ADK
// reads off the original.
//
// ReadonlyContext embeds context.Context, so the toolset's HTTP calls inherit
// whichever cancellation and deadline it carries. The merged fetch needs its
// own — the caller's values, none of the caller's cancellation, a deadline of
// its own — and this is the only seam where that can be substituted.
type withDeadlineCtx struct {
	agent.ReadonlyContext
	run context.Context
}

func (w withDeadlineCtx) Deadline() (time.Time, bool) { return w.run.Deadline() }
func (w withDeadlineCtx) Done() <-chan struct{}       { return w.run.Done() }
func (w withDeadlineCtx) Err() error                  { return w.run.Err() }

// Value still comes from the run context, which was derived from the original
// with WithoutCancel — so ADK's values are all still there.
func (w withDeadlineCtx) Value(key any) any { return w.run.Value(key) }

func withDeadline(orig agent.ReadonlyContext, run context.Context) agent.ReadonlyContext {
	if orig == nil || run == nil {
		return orig
	}
	return withDeadlineCtx{ReadonlyContext: orig, run: run}
}
