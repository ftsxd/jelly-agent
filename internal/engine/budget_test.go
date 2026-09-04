package engine

import (
	"strings"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/gateway"
)

func meta(inv string) gateway.CallMeta {
	return gateway.CallMeta{SessionID: "s1", InvocationID: inv, CallID: "c1"}
}

// 验收其一：小结果不被迫找回。
//
// This is the regression the whole design has to avoid. A fixed ceiling cut a
// 9947-byte listing and cost 68k tokens in retries; a dynamic budget that
// withheld small results on an empty prompt would repeat that with extra
// steps.
func TestASmallResultOnAFreshPromptIsPassedThroughWhole(t *testing.T) {
	b := newResultBudget()
	// 64k window, a 2k prompt, 8k reserved for the reply.
	b.observe("inv1", 2000, 64000, 8000)

	for _, size := range []int{50, 600, 2988, 9947} {
		if got := b.Allow(nil, meta("inv1"), size); got != size {
			t.Errorf("a %d-byte result was cut to %d on a nearly empty prompt", size, got)
		}
	}
}

// 验收其二：大结果不会直接撑爆请求。
func TestAResultLargerThanTheRoundsRoomIsWithheldWhole(t *testing.T) {
	b := newResultBudget()
	// A 64k window with a 50k prompt and 8k for the reply leaves 6k, and the
	// per-round share caps it at 16k anyway — so 6k is the room.
	b.observe("inv1", 50000, 64000, 8000)

	if got := b.Allow(nil, meta("inv1"), 41709); got != 0 {
		t.Errorf("a 41709-byte result got %d bytes of room on a nearly full prompt", got)
	}
	// Withheld whole, not cut: nothing was deducted, so a sibling that does
	// fit still gets through. A half-sent JSON object is what produced seven
	// pagination guesses.
	if got := b.Allow(nil, meta("inv1"), 1000); got != 1000 {
		t.Errorf("a small sibling call was refused %d/1000 after a large one was withheld", got)
	}
}

// 多工具一起返回时，要按合计大小判断。
//
// Each call individually fits; together they do not. Judging each against the
// full remaining budget is how eight parallel results each "fit" and the
// request then overruns the window.
func TestParallelResultsAreJudgedOnTheirCombinedSize(t *testing.T) {
	b := newResultBudget()
	b.observe("inv1", 0, 20000, 5000) // room = min(15000, 25% of 20000) = 5000

	granted, withheld := 0, 0
	for i := 0; i < 8; i++ {
		if got := b.Allow(nil, meta("inv1"), 1000); got == 1000 {
			granted++
		} else {
			withheld++
		}
	}
	if granted != 5 {
		t.Errorf("granted %d of 8 one-kilobyte results, want 5 — the room was 5000 bytes", granted)
	}
	if withheld != 3 {
		t.Errorf("withheld %d, want 3", withheld)
	}
}

// A new round resets the allowance: the previous round's results are now part
// of the prompt, and observe is told the new prompt size.
func TestEachRoundGetsAFreshAllowance(t *testing.T) {
	b := newResultBudget()
	b.observe("inv1", 0, 20000, 5000)
	if got := b.Allow(nil, meta("inv1"), 5000); got != 5000 {
		t.Fatalf("first round: %d", got)
	}
	if got := b.Allow(nil, meta("inv1"), 1); got != 0 {
		t.Fatalf("the round's room was spent but %d bytes were still granted", got)
	}
	// Next round: the prompt grew by what was just sent.
	b.observe("inv1", 5000, 20000, 5000)
	if got := b.Allow(nil, meta("inv1"), 4000); got != 4000 {
		t.Errorf("second round refused %d/4000 despite a fresh allowance", got)
	}
}

// An unknown context window means there is no budget to enforce. Guessing one
// would withhold payloads on a number nobody chose — and a wrong window is
// worse than an admittedly absent one.
func TestNoContextWindowMeansNoBudget(t *testing.T) {
	b := newResultBudget()
	b.observe("inv1", 1_000_000, 0, 0)
	if got := b.Allow(nil, meta("inv1"), 10_000_000); got != 10_000_000 {
		t.Errorf("a result was withheld with no configured window: %d", got)
	}
}

// A call from a round nobody observed must pass through, not be withheld.
// Defaulting to "withhold" would silently gut every path that does not run
// through the model callback.
func TestAnUnobservedRoundPassesThrough(t *testing.T) {
	b := newResultBudget()
	if got := b.Allow(nil, meta("never-seen"), 99999); got != 99999 {
		t.Errorf("an unobserved round was budgeted: %d", got)
	}
	if got := b.Allow(nil, gateway.CallMeta{}, 99999); got != 99999 {
		t.Errorf("a call with no invocation id was budgeted: %d", got)
	}
}

// A prompt already past the window leaves no room, and must not produce a
// negative allowance that reads as "unlimited".
func TestAnOverfullPromptLeavesNoRoomRatherThanNegativeRoom(t *testing.T) {
	b := newResultBudget()
	b.observe("inv1", 100000, 64000, 8000)
	if got := b.Allow(nil, meta("inv1"), 10); got != 0 {
		t.Errorf("room = %d on a prompt already over the window", got)
	}
	// Asserted on the stored quantity too, not only on the decision. Allow
	// happens to treat a negative room like zero, so without this the clamp
	// is untested code that merely looks careful.
	if room := b.byID["inv1"]; room != 0 {
		t.Errorf("stored room = %d; remaining room is not a quantity that can be negative", room)
	}
}

// The prompt estimate must include the system instruction and tool schemas.
// Counting only the messages reports a prompt as far smaller than the provider
// will bill, and the budget derived from it then admits results that do not fit.
func TestPromptEstimateCountsMoreThanTheMessages(t *testing.T) {
	msgs := promptTokensOf([]string{strings.Repeat("hello ", 100)})
	withSchemas := promptTokensOf([]string{
		strings.Repeat("hello ", 100),
		strings.Repeat("tool description ", 200),
	})
	if withSchemas <= msgs {
		t.Errorf("schemas added %d tokens", withSchemas-msgs)
	}
}
