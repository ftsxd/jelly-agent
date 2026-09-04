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

// pick is the turn's selection, split by the claim each tool has on a slot.
type pick struct {
	// Baseline tools are unconditional: a workflow stage runs them before the
	// model gets a turn, so they do not spend budget. The selector has said so
	// since it was written; folding them in with everything else broke that,
	// and with a budget of one a baseline tool displaced the very tool the
	// question had matched.
	baseline []string
	// Matched are the tools this question hit. Never displaced: a set that
	// lacks the tool the question needs has the model told it does not exist,
	// and answering anyway.
	matched []string
	// Filler scored nothing and is present only because the budget had room.
	// It yields to whatever the session already has — that yielding is what
	// keeps the prompt prefix stable across turns.
	filler []string
}

// admit decides the turn's tool set and remembers it.
//
// order maps a tool name to its position in the catalogue, and is also the
// authority on what still exists: a name missing from it has been retired, and
// carrying it forward would spend a slot on a tool the caller cannot resolve —
// which is worse than dropping it, because the slot buys nothing. With a
// budget of one that produced an empty tool list.
//
// The result is sorted by catalogue position, so an identical set always
// renders identically; otherwise two turns that chose the same tools would
// still miss the cache.
func (a *admissions) admit(session string, p pick, order map[string]int, budget int) []string {
	live := func(names []string) []string {
		out := make([]string, 0, len(names))
		for _, n := range names {
			if _, ok := order[n]; ok {
				out = append(out, n)
			}
		}
		return out
	}
	baseline, matched, filler := live(p.baseline), live(p.matched), live(p.filler)

	if session == "" {
		// No session to be stable across; nothing to remember.
		all := append(append(append([]string(nil), baseline...), matched...), filler...)
		return sortByCatalogue(dedupe(all), order)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	prev, seen := a.byID[session]
	if !seen {
		a.evictIfFullLocked()
		a.order = append(a.order, session)
	}
	prev = live(prev)

	// Baseline sits outside the budget, so the budget is what is left for
	// everything that has to compete for a slot.
	room := budget
	if room <= 0 {
		room = len(matched) + len(prev) + len(filler)
	}
	scored := make([]string, 0, room)
	take := func(names []string) {
		for _, n := range names {
			if len(scored) >= room || slices.Contains(scored, n) || slices.Contains(baseline, n) {
				continue
			}
			scored = append(scored, n)
		}
	}
	take(matched) // this question's needs come first
	take(prev)    // then keep the prompt as it was
	take(filler)  // only then spend what is left on padding

	// Remembered without the baseline tools: they are added unconditionally
	// every turn, so storing them would let them eat next turn's budget
	// through prev.
	a.byID[session] = scored
	return sortByCatalogue(dedupe(append(append([]string(nil), baseline...), scored...)), order)
}

func dedupe(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
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
