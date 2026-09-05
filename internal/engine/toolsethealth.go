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
func (h *toolsetHealth) skip(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	until, ok := h.down[name]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(h.down, name)
		return false
	}
	return true
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
