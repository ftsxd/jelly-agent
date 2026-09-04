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

	first := a.admit("s1", pick{matched: []string{"logs"}, filler: []string{"pad1", "pad2"}}, cat, 3)
	if !slices.Equal(first, []string{"pad1", "pad2", "logs"}) {
		t.Fatalf("first turn = %v", first)
	}

	second := a.admit("s1", pick{matched: []string{"sql"}, filler: []string{"pad1", "pad2"}}, cat, 3)
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
	third := a.admit("s1", pick{matched: []string{"logs"}, filler: []string{"pad1", "pad2"}}, cat, 3)
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
	got := a.admit("s1", pick{matched: []string{"mango", "zebra"}, filler: []string{"apple"}}, cat, 10)
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

	a.admit("s1", pick{matched: []string{"a", "b"}, filler: nil}, cat, 2)
	got := a.admit("s1", pick{matched: []string{"c", "d"}, filler: nil}, cat, 2)
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
	a.admit("s1", pick{matched: []string{"a"}, filler: nil}, cat, 10)
	got := a.admit("s2", pick{matched: []string{"b"}, filler: nil}, cat, 10)
	if !slices.Equal(got, []string{"b"}) {
		t.Errorf("s2 = %v, want only its own tools", got)
	}
}

// No session id means no conversation to be stable across. Sharing one bucket
// between unrelated runs would be worse than not remembering at all.
func TestNoSessionMeansNoStickiness(t *testing.T) {
	cat := catalogue("a", "b")
	a := newAdmissions()
	a.admit("", pick{matched: []string{"a"}, filler: nil}, cat, 10)
	got := a.admit("", pick{matched: []string{"b"}, filler: nil}, cat, 10)
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
		a.admit("s"+strconv.Itoa(i), pick{matched: []string{"a"}, filler: nil}, cat, 10)
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
	a.admit("s1", pick{matched: []string{"a"}, filler: nil}, cat, 10)
	a.forget("s1")
	got := a.admit("s1", pick{matched: []string{"b"}, filler: nil}, cat, 10)
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
			a.admit("s"+strconv.Itoa(i%3), pick{matched: []string{"a", "b"}, filler: nil}, cat, 10)
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// Baseline tools do not spend budget. The selector has said so since it was
// written — a baseline tool is one a workflow stage runs before the model gets
// a turn, so excluding it breaks the stage rather than narrowing a choice.
//
// Folding it in with everything else broke that invariant: with a budget of
// one, the baseline tool displaced the very tool the question had matched.
func TestBaselineDoesNotSpendBudget(t *testing.T) {
	cat := catalogue("collect_metrics", "get_logs")
	a := newAdmissions()
	got := a.admit("s1", pick{
		baseline: []string{"collect_metrics"},
		matched:  []string{"get_logs"},
	}, cat, 1)
	if !slices.Contains(got, "get_logs") {
		t.Errorf("got %v, want the matched tool — baseline must not take its slot", got)
	}
	if !slices.Contains(got, "collect_metrics") {
		t.Errorf("got %v, want the baseline tool", got)
	}
}

// A baseline tool must not accumulate into the standing set either, or it
// would come back next turn through the history and eat the budget it was
// supposed to sit outside of.
func TestBaselineIsNotRememberedAsAStandingTool(t *testing.T) {
	cat := catalogue("collect_metrics", "a", "b")
	a := newAdmissions()
	a.admit("s1", pick{baseline: []string{"collect_metrics"}, matched: []string{"a"}}, cat, 1)
	got := a.admit("s1", pick{baseline: []string{"collect_metrics"}, matched: []string{"b"}}, cat, 1)
	if !slices.Contains(got, "b") {
		t.Errorf("got %v, want the newly matched tool", got)
	}
	if !slices.Contains(got, "collect_metrics") {
		t.Errorf("got %v, want the baseline tool still present", got)
	}
}

// A tool that has been retired must not hold a slot. The catalogue is the
// authority on what exists; a name missing from it cannot be resolved by the
// caller, so the slot buys nothing — and with a budget of one it produced an
// empty tool list.
func TestRetiredToolsReleaseTheirSlot(t *testing.T) {
	a := newAdmissions()
	a.admit("s1", pick{matched: []string{"old_tool"}}, catalogue("old_tool"), 1)

	// old_tool is gone from the catalogue; new_tool is all there is.
	got := a.admit("s1", pick{filler: []string{"new_tool"}}, catalogue("new_tool"), 1)
	if !slices.Equal(got, []string{"new_tool"}) {
		t.Errorf("got %v, want [new_tool] — the retired tool must release its slot", got)
	}
}

// Retirement must also clear the remembered set, or the dead name keeps being
// filtered out on every later turn while still occupying the record.
func TestRetiredToolIsForgotten(t *testing.T) {
	a := newAdmissions()
	a.admit("s1", pick{matched: []string{"old_tool"}}, catalogue("old_tool"), 5)
	a.admit("s1", pick{matched: []string{"new_tool"}}, catalogue("new_tool"), 5)

	a.mu.Lock()
	kept := append([]string(nil), a.byID["s1"]...)
	a.mu.Unlock()
	if slices.Contains(kept, "old_tool") {
		t.Errorf("standing set = %v, want the retired tool dropped", kept)
	}
}

// admit takes three slices and must not assume they are disjoint or that
// every name still exists. The engine happens to hand it clean input today,
// which is exactly why these are worth pinning: a defensive branch nothing
// reaches is a branch nobody notices breaking.
func TestAdmitToleratesOverlappingAndStaleInput(t *testing.T) {
	cat := catalogue("collect_metrics", "get_logs")

	t.Run("a baseline name repeated in matched must not spend budget", func(t *testing.T) {
		a := newAdmissions()
		got := a.admit("s1", pick{
			baseline: []string{"collect_metrics"},
			matched:  []string{"collect_metrics", "get_logs"},
		}, cat, 1)
		if !slices.Contains(got, "get_logs") {
			t.Errorf("got %v; the repeated baseline name took the only slot", got)
		}
		if n := len(got); n != 2 {
			t.Errorf("got %v (%d entries), want the baseline plus one scored tool", got, n)
		}
	})

	t.Run("a name missing from the catalogue is dropped", func(t *testing.T) {
		a := newAdmissions()
		got := a.admit("s1", pick{
			matched: []string{"gone", "get_logs"},
			filler:  []string{"also_gone"},
		}, cat, 5)
		if slices.Contains(got, "gone") || slices.Contains(got, "also_gone") {
			t.Errorf("got %v, want only tools the catalogue still has", got)
		}
		if !slices.Contains(got, "get_logs") {
			t.Errorf("got %v, want the surviving tool", got)
		}
	})

	t.Run("a stale name must not consume a slot", func(t *testing.T) {
		a := newAdmissions()
		got := a.admit("s1", pick{matched: []string{"gone"}, filler: []string{"get_logs"}}, cat, 1)
		if !slices.Equal(got, []string{"get_logs"}) {
			t.Errorf("got %v, want [get_logs] — the stale name must release its slot", got)
		}
	})

	t.Run("duplicates never reach the output twice", func(t *testing.T) {
		a := newAdmissions()
		got := a.admit("s1", pick{
			matched: []string{"get_logs", "get_logs"},
			filler:  []string{"get_logs"},
		}, cat, 5)
		if !slices.Equal(got, []string{"get_logs"}) {
			t.Errorf("got %v, want a single entry", got)
		}
	})
}

// The standing set records the tools that competed for a slot, not the ones
// that sit outside the budget. Storing a baseline tool there would have it
// come back next turn as history and spend budget it is exempt from.
func TestStandingSetExcludesBaseline(t *testing.T) {
	cat := catalogue("collect_metrics", "get_logs")
	a := newAdmissions()
	a.admit("s1", pick{baseline: []string{"collect_metrics"}, matched: []string{"get_logs"}}, cat, 5)

	a.mu.Lock()
	kept := append([]string(nil), a.byID["s1"]...)
	a.mu.Unlock()
	if slices.Contains(kept, "collect_metrics") {
		t.Errorf("standing set = %v, want the baseline tool excluded", kept)
	}
	if !slices.Contains(kept, "get_logs") {
		t.Errorf("standing set = %v, want the scored tool recorded", kept)
	}
}
