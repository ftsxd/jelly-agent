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
	// observe, when set, is notified about every request — not only the ones
	// that were shortened.
	//
	// It fires unconditionally because the interesting number is often that
	// nothing was dropped: "the history was 4,000 tokens and none of it was
	// evicted" is what rules compaction out as the cause of a bad answer, and a
	// hook that only fires on change can never say it.
	//
	// The request is passed along so a caller can account for the parts
	// compaction does not touch (system instruction, tool schemas) without
	// wrapping the model a second time. This package still depends on neither a
	// logger nor a tracer.
	observe func(context.Context, *adkmodel.LLMRequest, Result)
}

// Wrap layers compaction over llm. Callers that want compaction off should not
// call Wrap at all — a zero Policy means "use the defaults", not "disabled".
func Wrap(llm adkmodel.LLM, pol Policy, observe func(context.Context, *adkmodel.LLMRequest, Result)) adkmodel.LLM {
	return &CompactingLLM{inner: llm, pol: pol.withDefaults(), observe: observe}
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
	if c.observe != nil {
		c.observe(ctx, req, res)
	}
	if !res.Changed() {
		return c.inner.GenerateContent(ctx, req, stream)
	}
	shallow := *req
	shallow.Contents = compacted
	return c.inner.GenerateContent(ctx, &shallow, stream)
}

var _ adkmodel.LLM = (*CompactingLLM)(nil)
