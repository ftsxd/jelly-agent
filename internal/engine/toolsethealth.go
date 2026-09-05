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

// toolsetHealth records which servers recently failed to answer.
type toolsetHealth struct {
	mu   sync.Mutex
	down map[string]time.Time // server name → when to try again
}

func newToolsetHealth() *toolsetHealth {
	return &toolsetHealth{down: map[string]time.Time{}}
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

// fail starts (or extends) a server's cooldown.
func (h *toolsetHealth) fail(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.down[name] = time.Now().Add(toolsetCooldown)
}

// ok clears a server's cooldown.
func (h *toolsetHealth) ok(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.down, name)
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
