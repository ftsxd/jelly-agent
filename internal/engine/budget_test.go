package engine

import (
	"strings"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/gateway"
)

func meta(inv string) gateway.CallMeta {
	return gateway.CallMeta{SessionID: "s1", InvocationID: inv, CallID: "c1"}
}

// payloadOf builds an ASCII payload of a known estimated token size.
//
// ASCII is four characters per token in the estimator, so the size is exact
// rather than approximate — these assertions are about the budget's
// arithmetic, and a fuzzy input would hide an off-by-a-lot.
func payloadOf(estTokens int) []byte {
	return []byte(strings.Repeat("x", estTokens*4))
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

	// Real sizes from a measured run: an empty listing, a preview, a business
	// group listing, and the alert listing that the old 8000-byte ceiling cut.
	for _, bytes := range []int{50, 600, 2988, 9947} {
		if !b.Fits(nil, meta("inv1"), []byte(strings.Repeat("x", bytes))) {
			t.Errorf("a %d-byte result was withheld on a nearly empty prompt", bytes)
		}
	}
}

// 验收其二：大结果不会直接撑爆请求。
func TestAResultLargerThanTheRoundsRoomIsWithheld(t *testing.T) {
	b := newResultBudget()
	// A 64k window with a 50k prompt and 8k for the reply leaves 6k tokens,
	// and the per-round share caps it at 16k anyway — so 6k is the room.
	b.observe("inv1", 50000, 64000, 8000)

	if b.Fits(nil, meta("inv1"), payloadOf(20000)) {
		t.Error("a 20k-token result was admitted with 6k tokens of room")
	}
	// Withheld whole, not cut, and nothing deducted — so a sibling that does
	// fit still gets through. A half-sent JSON object is what produced seven
	// pagination guesses.
	if !b.Fits(nil, meta("inv1"), payloadOf(1000)) {
		t.Error("a small sibling call was refused after a large one was withheld")
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

	admitted, withheld := 0, 0
	for i := 0; i < 8; i++ {
		if b.Fits(nil, meta("inv1"), payloadOf(1000)) {
			admitted++
		} else {
			withheld++
		}
	}
	if admitted != 5 {
		t.Errorf("admitted %d of 8 one-thousand-token results, want 5 — the room was 5000 tokens", admitted)
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
	if !b.Fits(nil, meta("inv1"), payloadOf(5000)) {
		t.Fatal("the round's whole room was refused")
	}
	if b.Fits(nil, meta("inv1"), payloadOf(1)) {
		t.Fatal("the round's room was spent but another payload was still admitted")
	}
	// Next round: the prompt grew by what was just sent.
	b.observe("inv1", 5000, 20000, 5000)
	if !b.Fits(nil, meta("inv1"), payloadOf(4000)) {
		t.Error("the second round refused a payload despite a fresh allowance")
	}
}

// The estimate must be the estimator's, not a bytes-per-token guess.
//
// The first version assumed one byte per token, which withheld ASCII payloads
// roughly four times sooner than necessary: a 40KB log is about 10k tokens,
// not 40k. This pins the arithmetic to the estimator that measures the prompt.
func TestTheBudgetMeasuresTokensNotBytes(t *testing.T) {
	b := newResultBudget()
	b.observe("inv1", 0, 40000, 10000) // room = min(30000, 25% of 40000) = 10000

	// 36KB of ASCII is about 9k tokens, so it fits a 10k-token room. A
	// byte-based budget would have refused it.
	if !b.Fits(nil, meta("inv1"), []byte(strings.Repeat("x", 36000))) {
		t.Error("36KB of ASCII (~9k tokens) was withheld from a 10k-token room")
	}
}

// An unknown context window means there is no budget to enforce. Guessing one
// would withhold payloads on a number nobody chose — and a wrong window is
// worse than an admittedly absent one.
func TestNoContextWindowMeansNoBudget(t *testing.T) {
	b := newResultBudget()
	b.observe("inv1", 1_000_000, 0, 0)
	if !b.Fits(nil, meta("inv1"), payloadOf(10_000_000)) {
		t.Error("a result was withheld with no configured window")
	}
}

// A call from a round nobody observed must pass through, not be withheld.
// Defaulting to "withhold" would silently gut every path that does not run
// through the model callback.
func TestAnUnobservedRoundPassesThrough(t *testing.T) {
	b := newResultBudget()
	if !b.Fits(nil, meta("never-seen"), payloadOf(99999)) {
		t.Error("an unobserved round was budgeted")
	}
	if !b.Fits(nil, gateway.CallMeta{}, payloadOf(99999)) {
		t.Error("a call with no invocation id was budgeted")
	}
}

// A prompt already past the window leaves no room, and must not produce a
// negative allowance that reads as "unlimited".
func TestAnOverfullPromptLeavesNoRoomRatherThanNegativeRoom(t *testing.T) {
	b := newResultBudget()
	b.observe("inv1", 100000, 64000, 8000)
	if b.Fits(nil, meta("inv1"), payloadOf(1)) {
		t.Error("a payload was admitted on a prompt already over the window")
	}
	// Asserted on the stored quantity too, not only on the decision. Fits
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
