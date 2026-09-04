package engine

// Keeping the tool set stable within a conversation.
//
// Tool schemas are the first thing a provider renders into the prompt — the
// order is tools, then the system instruction, then the messages — and prompt
// caching is a prefix match: one byte different at the front and everything
// after it is uncached. So a tool set that changes shape from turn to turn
// does not merely cost the difference in schema tokens; it forfeits the cache
// on the entire history behind it.
//
// That is a bad trade at any realistic size. Trimming a few hundred tokens of
// schema to lose a cache hit on ten thousand tokens of history is a loss of
// roughly an order of magnitude, because a cache read is a small fraction of
// the price of fresh input.
//
// So a slot is only spent on this turn's question when this turn's question
// actually needs it. Selection produces two kinds of tool: the ones the
// question hit, and filler — tools that scored nothing and are in the set only
// because the budget had room. Filler is where the instability came from, and
// filler is exactly what can be given away: a tool already in the prompt is at
// least as useful as an arbitrary one, and it costs nothing to send.
//
// Hence the order below: matched tools first, then whatever was already
// admitted, then this turn's filler. Anthropic's own tool search takes the
// same line from the other direction — it appends schemas rather than swapping
// them, specifically to preserve the prefix.
//
// A first attempt at this simply unioned the whole selection with the
// session's standing set and fell back to the fresh selection when the union
// overflowed. That looked monotone and achieved nothing: whenever the budget
// actually binds, the union always overflows, so it always fell back — and
// when the budget does not bind, the set is everything and was never unstable
// to begin with. The fix only works if filler yields.

import (
	"slices"
	"sync"
)

// maxAdmitSessions bounds the bookkeeping.
//
// The map would otherwise grow with every session the process ever serves. The
// cost of evicting an entry is one cache miss on that session's next turn, so
// the bound can be generous without being unbounded.
const maxAdmitSessions = 512

// admissions remembers which tools have entered the prompt, per session.
type admissions struct {
	mu    sync.Mutex
	byID  map[string][]string // session id → tool names
	order []string            // insertion order, for eviction
}

func newAdmissions() *admissions {
	return &admissions{byID: map[string][]string{}}
}

// admit decides the turn's tool set and remembers it.
//
// matched are the tools this question hit; they are never displaced, because a
// set that lacks the tool the question needs produces the failure this design
// exists to prevent — the model told the tool does not exist, answering
// anyway. filler are the rest of the selection, ranked best-first, and they
// yield to whatever the session already has.
//
// order maps a tool name to its position in the catalogue. The result is
// sorted by it, so an identical set always renders identically — otherwise two
// turns that chose the same tools would still miss the cache.
func (a *admissions) admit(session string, matched, filler []string, order map[string]int, budget int) []string {
	if session == "" {
		// No session to be stable across; nothing to remember.
		return sortByCatalogue(append(append([]string(nil), matched...), filler...), order)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	prev, seen := a.byID[session]
	if !seen {
		a.evictIfFullLocked()
		a.order = append(a.order, session)
	}

	room := budget
	if room <= 0 {
		room = len(matched) + len(prev) + len(filler)
	}
	final := make([]string, 0, room)
	take := func(names []string) {
		for _, n := range names {
			if len(final) >= room || slices.Contains(final, n) {
				continue
			}
			final = append(final, n)
		}
	}
	take(matched) // this question's needs come first
	take(prev)    // then keep the prompt as it was
	take(filler)  // only then spend what is left on padding

	a.byID[session] = final
	return sortByCatalogue(final, order)
}

// forget drops a session's bookkeeping.
func (a *admissions) forget(session string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.byID, session)
	a.order = slices.DeleteFunc(a.order, func(s string) bool { return s == session })
}

func (a *admissions) evictIfFullLocked() {
	for len(a.order) >= maxAdmitSessions && len(a.order) > 0 {
		oldest := a.order[0]
		a.order = a.order[1:]
		delete(a.byID, oldest)
	}
}

// sortByCatalogue renders a set in the catalogue's declared order.
//
// Deterministic presentation is the other half of prefix stability: the same
// set of tools has to serialize the same way every time, or the cache misses
// on a difference nobody chose.
func sortByCatalogue(names []string, order map[string]int) []string {
	out := append([]string(nil), names...)
	slices.SortStableFunc(out, func(x, y string) int {
		return order[x] - order[y]
	})
	return out
}
