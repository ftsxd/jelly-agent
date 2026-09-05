package engine

// How much room this round's prompt still has for tool results.
//
// The number that matters is not a tool's size, it is the space left in the
// request the results are about to join — and that depends on the
// conversation so far. A 40KB listing is fine on turn one and ruinous on turn
// twenty, and a fixed ceiling cannot tell those apart.
//
// The prompt size is taken from the before-model callback, which sees the
// request ADK is about to send. Tool calls come back from that request and
// their results are appended to it, so the size seen there is exactly the
// baseline the results have to fit on top of.
//
// Two limits are subtracted from the context window: what the prompt already
// occupies, and what the reply needs. The reply reservation is not optional —
// filling the window with input leaves the model unable to answer, which
// presents as a truncated or empty completion rather than as an error.
//
// One honest caveat. Parallel tool calls are granted in arrival order, and
// with concurrency that order is not deterministic, so which of two
// simultaneous large results gets through can vary between otherwise
// identical runs. The alternative — hold every result until the round's calls
// have all answered, then decide together — would serialize the parallelism
// that makes those calls worth issuing. Granting in arrival order never
// overruns the budget, which is the property that actually matters; which of
// two oversized results wins is not something a caller should depend on
// either way.

import (
	"context"
	"encoding/json"
	"sync"

	adkmodel "google.golang.org/adk/model"

	jellymodel "github.com/jelly-agent/jelly-agent/internal/model"

	"github.com/jelly-agent/jelly-agent/internal/gateway"
	"github.com/jelly-agent/jelly-agent/internal/tokens"
)

// Fractions of the context window.
const (
	// replyShare is reserved for the model's answer when the provider states
	// no max_tokens. A diagnosis with a table in it runs to a few thousand
	// tokens, and running out mid-answer wastes the whole turn.
	replyShare = 0.15
	// resultShare bounds what one round's tool results may take of the window
	// even when the prompt is nearly empty.
	//
	// Without it, turn one would hand the model the entire window in a single
	// result — technically a fit, and then every later turn re-sends it. The
	// cost of a large result is not paid once; it is paid on every subsequent
	// round of the same conversation, at full price whenever the cache misses.
	resultShare = 0.25
)

// resultBudget tracks, per invocation, how much room tool results have left.
type resultBudget struct {
	mu sync.Mutex
	// byID holds the remaining byte allowance for a round. A round is one
	// model request and the tool calls it produced.
	byID  map[string]int
	order []string
}

// maxBudgetRounds bounds the bookkeeping. An invocation that dies between the
// callback and its tools would otherwise leak an entry.
const maxBudgetRounds = 256

func newResultBudget() *resultBudget {
	return &resultBudget{byID: map[string]int{}}
}

// observe records the prompt about to be sent and resets the round's
// allowance. contextWindow of zero means the provider did not say, in which
// case there is no budget to enforce and the entry is cleared: guessing a
// window is worse than not having one, because it would withhold payloads on
// a number nobody chose.
func (b *resultBudget) observe(invocationID string, promptTokens, contextWindow, replyTokens int) {
	if invocationID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if contextWindow <= 0 {
		delete(b.byID, invocationID)
		return
	}
	if replyTokens <= 0 {
		replyTokens = int(float64(contextWindow) * replyShare)
	}
	room := contextWindow - promptTokens - replyTokens
	if cap := int(float64(contextWindow) * resultShare); room > cap {
		room = cap
	}
	// Clamped rather than left negative. Allow would treat a negative room the
	// same as zero, so this changes no decision today — but a stored negative
	// is a number a later reader would have to know not to trust, and
	// "remaining room" that can be less than nothing is not a quantity.
	if room < 0 {
		room = 0
	}
	if _, seen := b.byID[invocationID]; !seen {
		b.evictIfFullLocked()
		b.order = append(b.order, invocationID)
	}
	b.byID[invocationID] = room
}

// Fits implements gateway.ResultBudget.
//
// Room and payload are both measured in tokens by the same estimator, so
// there is no bytes-per-token ratio to get wrong. The first version had one,
// set to 1, and was therefore three to four times more pessimistic than the
// estimator it stood in for — a CJK rune is three bytes for one token and
// four ASCII characters are four bytes for one.
func (b *resultBudget) Fits(_ context.Context, meta gateway.CallMeta, payload []byte) bool {
	if meta.InvocationID == "" {
		return true
	}
	want := tokens.EstimateBytes(payload)

	b.mu.Lock()
	defer b.mu.Unlock()

	room, tracked := b.byID[meta.InvocationID]
	if !tracked {
		// No observation for this round — no window configured, or a call
		// that arrived outside one. Not a licence to withhold.
		return true
	}
	if want <= room {
		b.byID[meta.InvocationID] = room - want
		return true
	}
	// Does not fit. Nothing is deducted here: the caller will come back with
	// the smaller shape it actually sends and charge that, and a sibling call
	// in the same round that does fit should still get through.
	return false
}

// Cost implements gateway.ResultBudget.
func (b *resultBudget) Cost(payload []byte) int { return tokens.EstimateBytes(payload) }

// Charge implements gateway.ResultBudget.
//
// Allowed to drive the room negative — clamping would say there is space when
// there is not, and the next call in the round has to see the truth. observe
// clamps at the start of each round, so a deficit does not carry forward.
func (b *resultBudget) Charge(_ context.Context, meta gateway.CallMeta, payload []byte) {
	if meta.InvocationID == "" {
		return
	}
	spend := tokens.EstimateBytes(payload)
	b.mu.Lock()
	defer b.mu.Unlock()
	if room, tracked := b.byID[meta.InvocationID]; tracked {
		b.byID[meta.InvocationID] = room - spend
	}
}

func (b *resultBudget) evictIfFullLocked() {
	for len(b.order) >= maxBudgetRounds && len(b.order) > 0 {
		oldest := b.order[0]
		b.order = b.order[1:]
		delete(b.byID, oldest)
	}
}

// promptTokensOf estimates what a request occupies.
//
// Approximate on purpose, and the approximation is safe in the direction that
// matters: the budget it feeds withholds a payload the model can still fetch,
// so being off by a fifth costs a search rather than a wrong answer.
func promptTokensOf(texts []string) int {
	n := 0
	for _, t := range texts {
		n += tokens.Estimate(t)
	}
	return n
}

// requestTexts pulls the billable text out of a request.
//
// System instruction and tool declarations are included, not just the
// conversation: they are re-sent on every call and are a large share of a
// prompt with several dozen tools in it. Counting only the messages would
// report a prompt as far smaller than the provider will bill, and the budget
// derived from it would then admit a result that does not actually fit.
func requestTexts(req *adkmodel.LLMRequest) []string {
	if req == nil {
		return nil
	}
	var out []string
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			if p.Text != "" {
				out = append(out, p.Text)
			}
			if p.FunctionCall != nil {
				out = append(out, p.FunctionCall.Name)
				if b, err := json.Marshal(p.FunctionCall.Args); err == nil {
					out = append(out, string(b))
				}
			}
			if p.FunctionResponse != nil {
				out = append(out, p.FunctionResponse.Name)
				if b, err := json.Marshal(p.FunctionResponse.Response); err == nil {
					out = append(out, string(b))
				}
			}
		}
	}
	if cfg := req.Config; cfg != nil {
		if si := cfg.SystemInstruction; si != nil {
			for _, p := range si.Parts {
				if p != nil && p.Text != "" {
					out = append(out, p.Text)
				}
			}
		}
		for _, t := range cfg.Tools {
			for _, d := range t.FunctionDeclarations {
				if d == nil {
					continue
				}
				out = append(out, d.Name, d.Description)
				// The same selection the wire format makes, not a guess at
				// it: counting only Parameters missed the whole schema of
				// every tool that carries the JSON-schema form, which is what
				// functiontool emits and what MCP tools arrive as.
				if b, err := json.Marshal(jellymodel.ToolParameters(d)); err == nil {
					out = append(out, string(b))
				}
			}
		}
	}
	return out
}
