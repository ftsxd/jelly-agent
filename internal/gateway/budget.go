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

import "context"

// ResultBudget reports how much room a result has in the prompt.
//
// Optional: a nil budget means no dynamic decision, which is the behaviour
// this had before — the operator's explicit ceiling, or nothing.
type ResultBudget interface {
	// Allow reports how many bytes of payload may reach the prompt for this
	// call. It is at most size. Zero means the payload does not fit and only
	// an overview should be sent.
	//
	// Implementations must deduct what they grant, so that several tools
	// answering in the same round are judged on their combined size rather
	// than each against the whole remaining budget.
	Allow(ctx context.Context, meta CallMeta, size int) int
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
