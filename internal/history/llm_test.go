package history

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// spyLLM records the request it was handed.
type spyLLM struct {
	got   *adkmodel.LLMRequest
	calls int
}

func (s *spyLLM) Name() string { return "spy" }

func (s *spyLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	s.got = req
	s.calls++
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{}, nil)
	}
}

func drain(seq iter.Seq2[*adkmodel.LLMResponse, error]) {
	for range seq {
		break
	}
}

func TestCompactingLLMPassesShortHistoryThrough(t *testing.T) {
	spy := &spyLLM{}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{textContent(genai.RoleUser, "你好")}}
	llm := Wrap(spy, Policy{MaxTokens: 10000}, nil)

	drain(llm.GenerateContent(context.Background(), req, false))
	if spy.got != req {
		t.Error("an unchanged history should be forwarded as the same request, without copying")
	}
}

func TestCompactingLLMDoesNotMutateRequest(t *testing.T) {
	var in []*genai.Content
	for range 20 {
		in = append(in, textContent(genai.RoleUser, strings.Repeat("长", 300)))
	}
	req := &adkmodel.LLMRequest{Contents: in}
	originalLen := len(req.Contents)

	var seen Result
	spy := &spyLLM{}
	llm := Wrap(spy, Policy{MaxTokens: 400, KeepRecent: 2}, func(_ context.Context, _ *adkmodel.LLMRequest, r Result) { seen = r })
	drain(llm.GenerateContent(context.Background(), req, false))

	if len(req.Contents) != originalLen {
		t.Errorf("caller's request was rewritten: %d → %d contents", originalLen, len(req.Contents))
	}
	if spy.got == req {
		t.Error("compacted request must be a copy, not the caller's own request")
	}
	if len(spy.got.Contents) >= originalLen {
		t.Errorf("forwarded %d contents, expected fewer than %d", len(spy.got.Contents), originalLen)
	}
	if seen.Dropped == 0 {
		t.Error("observe hook was not told about the drops")
	}
}

func TestCompactingLLMPreservesOtherRequestFields(t *testing.T) {
	var in []*genai.Content
	for range 20 {
		in = append(in, textContent(genai.RoleUser, strings.Repeat("长", 300)))
	}
	req := &adkmodel.LLMRequest{
		Model:    "some-model",
		Contents: in,
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("be brief", genai.RoleUser),
		},
	}
	spy := &spyLLM{}
	drain(Wrap(spy, Policy{MaxTokens: 400, KeepRecent: 2}, nil).GenerateContent(context.Background(), req, false))

	if spy.got.Model != "some-model" {
		t.Errorf("Model lost: %q", spy.got.Model)
	}
	if spy.got.Config == nil || spy.got.Config.SystemInstruction == nil {
		t.Error("system instruction lost — compaction must only touch Contents")
	}
}

func TestCompactingLLMHandlesNilRequest(t *testing.T) {
	spy := &spyLLM{}
	drain(Wrap(spy, Policy{}, nil).GenerateContent(context.Background(), nil, false))
	if spy.calls != 1 {
		t.Error("nil request should be forwarded rather than panic")
	}
}

// The hook fires on every request, not only the compacted ones.
//
// "The history was N tokens and none of it was evicted" is what rules
// compaction out as the cause of a bad answer, and a hook that only fires on
// change can never say it. This is the behaviour the tracing layer depends on:
// the prompt breakdown has to appear on every span, not just the trimmed ones.
func TestObserveHookFiresWhenNothingWasCompacted(t *testing.T) {
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{
		textContent(genai.RoleUser, "短"),
	}}

	var calls int
	var seen Result
	llm := Wrap(&spyLLM{}, Policy{MaxTokens: 100_000}, func(_ context.Context, r *adkmodel.LLMRequest, res Result) {
		calls++
		seen = res
		if r != req {
			t.Error("hook should receive the caller's own request, before any copy")
		}
	})
	drain(llm.GenerateContent(context.Background(), req, false))

	if calls != 1 {
		t.Fatalf("hook fired %d times, want 1", calls)
	}
	if seen.Changed() {
		t.Errorf("Changed() = true for an untouched history: %+v", seen)
	}
	if seen.BeforeTokens == 0 {
		t.Error("BeforeTokens = 0; the history size must be reported even when nothing was dropped")
	}
	if seen.AfterTokens != seen.BeforeTokens {
		t.Errorf("AfterTokens = %d, want %d when nothing was compacted", seen.AfterTokens, seen.BeforeTokens)
	}
}
