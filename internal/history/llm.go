package history

import (
	"context"
	"iter"

	adkmodel "google.golang.org/adk/model"
)

// CompactingLLM wraps a model.LLM and shrinks the conversation to fit the
// policy before each call.
//
// It sits at the model boundary because that is the one place the full request
// is visible — but it deliberately lives outside internal/model, which stays a
// pure protocol translator. Conversation policy belongs here.
type CompactingLLM struct {
	inner adkmodel.LLM
	pol   Policy
	// onCompact, when set, is notified after a request was shortened. Used for
	// logging; kept as a hook so this package does not depend on a logger.
	onCompact func(Result)
}

// Wrap layers compaction over llm. Callers that want compaction off should not
// call Wrap at all — a zero Policy means "use the defaults", not "disabled".
func Wrap(llm adkmodel.LLM, pol Policy, onCompact func(Result)) adkmodel.LLM {
	return &CompactingLLM{inner: llm, pol: pol.withDefaults(), onCompact: onCompact}
}

// Name implements model.LLM.
func (c *CompactingLLM) Name() string { return c.inner.Name() }

// GenerateContent implements model.LLM, compacting req's history first.
//
// The incoming request is never modified: ADK holds on to these objects for
// session state and the live stream, so a shortened history is delivered via a
// shallow copy of the request pointing at a fresh Contents slice.
func (c *CompactingLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	if req == nil {
		return c.inner.GenerateContent(ctx, req, stream)
	}
	compacted, res := Compact(req.Contents, c.pol)
	if !res.Changed() {
		return c.inner.GenerateContent(ctx, req, stream)
	}
	if c.onCompact != nil {
		c.onCompact(res)
	}
	shallow := *req
	shallow.Contents = compacted
	return c.inner.GenerateContent(ctx, &shallow, stream)
}

var _ adkmodel.LLM = (*CompactingLLM)(nil)
