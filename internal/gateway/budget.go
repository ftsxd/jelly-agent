package gateway

// Deciding how much of a result may reach the prompt.
//
// A fixed byte ceiling is the wrong instrument, and there is a measurement
// behind that. An 8000-byte ceiling once cut a 9947-byte listing in half; the
// model could not parse the fragment, retried seven pagination guesses, and
// the turn cost 68k tokens to save two kilobytes. Every retry re-sends the
// whole history, so a ceiling that makes a result unusable costs far more than
// the bytes it saved. The default is therefore no fixed ceiling at all.
//
// But "no ceiling" is only right while the result fits. What actually matters
// is not the result's size, it is whether the prompt it is about to join still
// has room — and that depends on the conversation so far, not on the tool. So
// the question asked here is the live one: given what this round's prompt
// already occupies and the space the reply needs, how much of this result can
// go in.
//
// When it does not fit, the payload is withheld whole rather than cut. A cut
// JSON object is worse than an absent one: the model cannot parse it, cannot
// tell which records are missing, and — as the run above showed — starts
// guessing at pagination. An overview plus a small preview plus a handle it
// can search is strictly more useful and much smaller.

import (
	"context"
	"encoding/json"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// ResultBudget reports how much room a result has in the prompt.
//
// Optional: a nil budget means no dynamic decision, which is the behaviour
// this had before — the operator's explicit ceiling, or nothing.
type ResultBudget interface {
	// Fits reports whether this payload may reach the prompt for this call.
	//
	// The payload itself is passed rather than its size, so the decision is
	// made on a token estimate of the actual bytes instead of on a
	// bytes-per-token ratio. That ratio is not a small detail: the estimator
	// counts a CJK rune as one token and four ASCII characters as one, so the
	// real range is three to four bytes per token, and a budget assuming one
	// withholds payloads three to four times sooner than it needs to.
	//
	// Implementations must deduct what they admit, so that several tools
	// answering in the same round are judged on their combined size rather
	// than each against the whole remaining budget.
	Fits(ctx context.Context, meta CallMeta, payload []byte) bool

	// Cost reports what a response would spend, in the same unit the budget
	// is kept in.
	//
	// Exposed so that choosing between candidate shapes uses the measure that
	// will actually be charged. Ranking them by byte length instead was
	// wrong in a way that only shows up on mixed content: the estimator
	// counts four ASCII characters as one token and a CJK rune as one, so a
	// response whose bulk is an ASCII payload and one whose bulk is a Chinese
	// explanatory note rank differently by bytes than by tokens — and the
	// smaller-by-bytes candidate was being sent while the larger-by-tokens
	// one was charged.
	Cost(payload []byte) int

	// Charge deducts unconditionally, for what goes into the prompt whether
	// it fits or not.
	//
	// Something always goes: even a withheld result sends a summary, a
	// handle, an overview and the note explaining them. Letting that ride
	// free is the same mistake as measuring only the payload — several
	// withheld results in one round would each cost nothing on paper and put
	// the request over the window in fact.
	Charge(ctx context.Context, meta CallMeta, payload []byte)
}

// maxPreviewBytes bounds the preview sent in place of a withheld payload.
//
// Small on purpose. Its only job is to let the model recognise the shape of
// what it is looking at — field names, the flavour of the records — so it can
// write a useful search pattern. Anything bigger starts re-creating the
// problem it exists to avoid.
const maxPreviewBytes = 600

// previewOf renders a small head of a payload.
//
// It cuts on a rune boundary and does not pretend the fragment is JSON: it is
// handed back as a string under its own key, never as `data`. Emitting it as
// `data` is what the old truncation did, and a model handed half an object
// treats it as the object — which is how a cut listing became seven pagination
// guesses.
func previewOf(payload []byte) string {
	r := []rune(string(payload))
	if len(r) <= maxPreviewBytes {
		return string(r)
	}
	return string(r[:maxPreviewBytes])
}

// payloadBytes renders what the model will receive, for measuring.
//
// The payload's own bytes are appended rather than round-tripped through the
// encoder: re-marshalling a multi-megabyte result on every call to weigh it
// would cost more than the budget saves, and ev.Data is already compact JSON,
// so the character mix a token estimate depends on is the same either way.
func payloadBytes(ev *ops.Evidence) []byte {
	out := toolPayload(ev)
	data, _ := out["data"]
	delete(out, "data")
	envelope, err := json.Marshal(out)
	if err != nil {
		// Unmeasurable is not free: fall back to the payload alone rather
		// than to zero, which would admit anything.
		return ev.Data
	}
	_ = data
	if len(ev.Data) == 0 {
		return envelope
	}
	return append(envelope, ev.Data...)
}

// fitResponse picks the largest shape of this result that the round can hold.
//
// Three candidates, in order of how much they tell the model: the payload
// itself, an overview with a preview, and an overview alone. The first that
// fits is sent.
//
// Choosing by measured size rather than by rule matters, because withholding
// does not always shrink anything. A small payload wrapped in a preview, an
// overview and the note explaining them comes out larger than the payload it
// replaced — measured at 454 tokens against 317 for a four-hundred-character
// result. Withholding it would cost the round more and tell the model less.
// So when nothing fits, what goes is whichever candidate is actually smallest,
// and it is charged, because it reaches the prompt whether the budget likes it
// or not.
func (g *Gateway) fitResponse(ctx context.Context, meta CallMeta, ev *ops.Evidence) {
	full := *ev

	held := *ev
	held.Preview = previewOf(ev.Data)
	held.Data = nil
	held.Withheld = true

	// The preview is the one part that can go: it is a convenience for writing
	// a search pattern, while the handle is what makes the result recoverable
	// at all.
	bare := held
	bare.Preview = ""

	candidates := []*ops.Evidence{&full, &held, &bare}
	rendered := make([][]byte, len(candidates))
	cost := make([]int, len(candidates))
	for i, c := range candidates {
		rendered[i] = payloadBytes(c)
		cost[i] = g.budget.Cost(rendered[i])
	}
	for i, c := range candidates {
		if g.budget.Fits(ctx, meta, rendered[i]) {
			*ev = *c
			return
		}
	}

	// Nothing fits, so the cheapest goes — measured in what will be charged,
	// not in bytes.
	smallest := 0
	for i := range cost {
		if cost[i] < cost[smallest] {
			smallest = i
		}
	}
	*ev = *candidates[smallest]
	g.budget.Charge(ctx, meta, rendered[smallest])
}
