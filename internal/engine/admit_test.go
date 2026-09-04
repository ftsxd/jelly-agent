package engine

import (
	"slices"
	"strconv"
	"testing"
)

func catalogue(names ...string) map[string]int {
	out := make(map[string]int, len(names))
	for i, n := range names {
		out[n] = i
	}
	return out
}

// Turn two must not reshape the prompt just because it asked something
// slightly different. Tool schemas render before the system prompt and the
// messages, and prompt caching is a prefix match, so a changed tool set
// forfeits the cache on the entire history behind it — paying ten thousand
// tokens of fresh input to save a few hundred of schema.
// The case that matters: the budget is binding, so every turn has to give
// something up, and what it gives up must be padding rather than the prompt's
// existing shape.
//
// Four tools, three slots. Turn one matches "logs" and pads with two others;
// turn two matches "sql". Without the standing set, turn two would drop
// "logs" — a changed prefix, and the whole history behind it uncached.
func TestFillerYieldsToWhatIsAlreadyInThePrompt(t *testing.T) {
	cat := catalogue("pad1", "pad2", "logs", "sql")
	a := newAdmissions()

	first := a.admit("s1", []string{"logs"}, []string{"pad1", "pad2"}, cat, 3)
	if !slices.Equal(first, []string{"pad1", "pad2", "logs"}) {
		t.Fatalf("first turn = %v", first)
	}

	second := a.admit("s1", []string{"sql"}, []string{"pad1", "pad2"}, cat, 3)
	if !slices.Contains(second, "sql") {
		t.Fatalf("second turn = %v, want the newly matched tool", second)
	}
	if !slices.Contains(second, "logs") {
		t.Errorf("second turn = %v, want the previous turn's tool kept over padding", second)
	}
	if len(second) > 3 {
		t.Errorf("second turn = %v, over the budget of 3", second)
	}

	// And it has settled: a third turn matching either one renders the same set.
	third := a.admit("s1", []string{"logs"}, []string{"pad1", "pad2"}, cat, 3)
	if !slices.Equal(third, second) {
		t.Errorf("third turn = %v, want it identical to %v", third, second)
	}
}

// The same set must serialize identically every time, or the cache misses on a
// difference nobody chose. Selection order is not presentation order.
// The catalogue's order, not the alphabet's and not the ranking's. Names
// chosen so the three orders differ: an earlier version used a/b/c, where
// catalogue order and alphabetical order coincide, and the test could not tell
// a correct implementation from a sort by name.
func TestPresentationIsAlwaysCatalogueOrder(t *testing.T) {
	cat := catalogue("zebra", "apple", "mango")
	a := newAdmissions()
	got := a.admit("s1", []string{"mango", "zebra"}, []string{"apple"}, cat, 10)
	if !slices.Equal(got, []string{"zebra", "apple", "mango"}) {
		t.Errorf("got %v, want catalogue order (zebra, apple, mango)", got)
	}
}

// Stability is not worth a wrong answer. Once the budget is full, a tool the
// current question needs must still get in — a stale set that lacks it would
// have the model told the tool does not exist, and answer anyway.
// Stability never costs correctness. When the standing set fills the budget
// and this question needs something else, the match wins — a set that lacks
// the tool the question needs has the model told the tool does not exist, and
// it answers anyway.
func TestAMatchIsNeverDisplacedByHistory(t *testing.T) {
	cat := catalogue("a", "b", "c", "d")
	a := newAdmissions()

	a.admit("s1", []string{"a", "b"}, nil, cat, 2)
	got := a.admit("s1", []string{"c", "d"}, nil, cat, 2)
	if !slices.Contains(got, "c") || !slices.Contains(got, "d") {
		t.Fatalf("got %v, want both newly matched tools", got)
	}
	if len(got) > 2 {
		t.Errorf("got %v, over the budget of 2", got)
	}
}

// Sessions must not share a set. Two conversations about different things
// would otherwise each carry the other's tools.
func TestSessionsAreIndependent(t *testing.T) {
	cat := catalogue("a", "b", "c")
	a := newAdmissions()
	a.admit("s1", []string{"a"}, nil, cat, 10)
	got := a.admit("s2", []string{"b"}, nil, cat, 10)
	if !slices.Equal(got, []string{"b"}) {
		t.Errorf("s2 = %v, want only its own tools", got)
	}
}

// No session id means no conversation to be stable across. Sharing one bucket
// between unrelated runs would be worse than not remembering at all.
func TestNoSessionMeansNoStickiness(t *testing.T) {
	cat := catalogue("a", "b")
	a := newAdmissions()
	a.admit("", []string{"a"}, nil, cat, 10)
	got := a.admit("", []string{"b"}, nil, cat, 10)
	if !slices.Equal(got, []string{"b"}) {
		t.Errorf("got %v, want no carry-over without a session", got)
	}
}

// The record is bounded. Without eviction it grows with every session the
// process ever serves.
func TestOldSessionsAreEvicted(t *testing.T) {
	cat := catalogue("a")
	a := newAdmissions()
	for i := 0; i < maxAdmitSessions+50; i++ {
		a.admit("s"+strconv.Itoa(i), []string{"a"}, nil, cat, 10)
	}
	a.mu.Lock()
	n := len(a.byID)
	a.mu.Unlock()
	if n > maxAdmitSessions {
		t.Errorf("kept %d sessions, want at most %d", n, maxAdmitSessions)
	}
}

func TestForgetDropsASession(t *testing.T) {
	cat := catalogue("a", "b")
	a := newAdmissions()
	a.admit("s1", []string{"a"}, nil, cat, 10)
	a.forget("s1")
	got := a.admit("s1", []string{"b"}, nil, cat, 10)
	if !slices.Equal(got, []string{"b"}) {
		t.Errorf("got %v, want the session forgotten", got)
	}
}

// Concurrent turns from different sessions are ordinary in a server.
func TestAdmitIsConcurrencySafe(t *testing.T) {
	cat := catalogue("a", "b", "c")
	a := newAdmissions()
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			a.admit("s"+strconv.Itoa(i%3), []string{"a", "b"}, nil, cat, 10)
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}
