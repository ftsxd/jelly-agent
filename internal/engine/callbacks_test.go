package engine

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/adk/agent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	jellymetrics "github.com/jelly-agent/jelly-agent/internal/metrics"
	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// fakeToolCtx supplies only the identifiers the telemetry callbacks read. Every
// other method of the interface panics (StrictContextMock), so a callback that
// starts reaching for more state fails this test loudly instead of quietly
// depending on something the real hook may not have.
type fakeToolCtx struct {
	agent.StrictContextMock
	callID string
}

func (f *fakeToolCtx) FunctionCallID() string { return f.callID }
func (f *fakeToolCtx) SessionID() string      { return "session-1" }
func (f *fakeToolCtx) InvocationID() string   { return "invocation-1" }
func (f *fakeToolCtx) AgentName() string      { return "jelly" }

type fakeTool struct{ name string }

func (f fakeTool) Name() string        { return f.name }
func (f fakeTool) Description() string { return "" }
func (f fakeTool) IsLongRunning() bool { return false }

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	e := New(&config.Config{})
	// Keep the test off the developer's real ~/.jelly-agent/state.db: a
	// tracker with no store still times calls and drops the rows, which is
	// exactly the path this test needs to exercise.
	e.metricsOnce.Do(func() { e.metrics = jellymetrics.NewTracker(nil) })
	t.Cleanup(e.Close)
	return e
}

// The one invariant that matters here. ADK reads a non-nil return from
// BeforeToolCallback as "skip the tool, use this result", and from
// AfterToolCallback as "replace the tool's output". A measurement hook that
// ever returns a value would silently swallow real tool calls — a bug that
// presents as a model regression, not as a telemetry bug.
func TestToolCallbacksNeverAlterExecution(t *testing.T) {
	e := newTestEngine(t)
	before, after := e.toolCallbacks()
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("callbacks = %d before / %d after, want 1 each", len(before), len(after))
	}

	ctx := &fakeToolCtx{StrictContextMock: agent.StrictContextMock{Ctx: context.Background()}, callID: "c1"}
	tl := fakeTool{name: "k8s_get_pods"}
	args := map[string]any{"ns": "payment"}

	got, err := before[0](ctx, tl, args)
	if got != nil || err != nil {
		t.Fatalf("before = (%v, %v), want (nil, nil) — a non-nil result skips the tool", got, err)
	}

	cases := []struct {
		name   string
		result map[string]any
		err    error
	}{
		{"success", map[string]any{"pods": "6/6 Running"}, nil},
		{"payload error", map[string]any{"error": "connection refused"}, nil},
		{"transport error", nil, errors.New("boom")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := after[0](ctx, tl, args, tc.result, tc.err)
			if got != nil || err != nil {
				t.Fatalf("after = (%v, %v), want (nil, nil) — a non-nil return replaces the tool output", got, err)
			}
		})
	}
}

// The before/after pair must actually meet, otherwise latency is never
// recorded and the in-flight table leaks.
func TestToolCallbacksPairStartAndFinish(t *testing.T) {
	e := newTestEngine(t)
	before, after := e.toolCallbacks()
	ctx := &fakeToolCtx{StrictContextMock: agent.StrictContextMock{Ctx: context.Background()}, callID: "c2"}
	tl := fakeTool{name: "web_search"}

	if _, err := before[0](ctx, tl, nil); err != nil {
		t.Fatalf("before: %v", err)
	}
	if got := e.Metrics().Pending(); got != 1 {
		t.Fatalf("Pending = %d after before(), want 1", got)
	}
	if _, err := after[0](ctx, tl, nil, map[string]any{"ok": true}, nil); err != nil {
		t.Fatalf("after: %v", err)
	}
	if got := e.Metrics().Pending(); got != 0 {
		t.Errorf("Pending = %d after after(), want 0", got)
	}
}

type fakeCallbackCtx struct {
	agent.StrictContextMock
}

func (f *fakeCallbackCtx) InvocationID() string { return "invocation-1" }
func (f *fakeCallbackCtx) SessionID() string    { return "session-1" }
func (f *fakeCallbackCtx) AgentName() string    { return "jelly" }

// The model hooks carry the same hazard as the tool hooks: ADK reads a non-nil
// return as "use this response instead", so a measurement hook that returns a
// value replaces the model's answer with nothing. That failure looks like the
// model going silent, not like a telemetry bug.
func TestModelCallbacksNeverAlterExecution(t *testing.T) {
	e := newTestEngine(t)
	before, after := e.modelCallbacks("deepseek-v4-flash")
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("callbacks = %d before / %d after, want 1 each", len(before), len(after))
	}

	ctx := &fakeCallbackCtx{agent.StrictContextMock{Ctx: context.Background()}}

	got, err := before[0](ctx, &adkmodel.LLMRequest{Model: "deepseek-v4-flash"})
	if got != nil || err != nil {
		t.Fatalf("before = (%v, %v), want (nil, nil) — a non-nil result replaces the model call", got, err)
	}

	cases := []struct {
		name string
		resp *adkmodel.LLMResponse
		err  error
	}{
		{"with usage", &adkmodel.LLMResponse{
			ModelVersion:  "deepseek-v4-flash-0925",
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 4061, CandidatesTokenCount: 647},
		}, nil},
		{"no usage metadata", &adkmodel.LLMResponse{}, nil},
		{"nil response", nil, errors.New("upstream 503")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := after[0](ctx, tc.resp, tc.err)
			if got != nil || err != nil {
				t.Fatalf("after = (%v, %v), want (nil, nil) — a non-nil return replaces the response", got, err)
			}
		})
	}
}

// A fixed widest ceiling made the policy unable to deny anything, which made
// the whole mechanism decoration. Derived from configuration, a deployment
// that never enables scripts gets a policy that would refuse one.
func TestSideEffectCeilingFollowsConfiguration(t *testing.T) {
	cfg := &config.Config{}
	cfg.Memory.Core.Dir = t.TempDir()

	e := New(cfg)
	if got := e.sideEffectCeiling(); got != ops.SideEffectMutating {
		t.Errorf("ceiling = %q with scripts off, want %q — remember/forget need mutating, nothing needs more",
			got, ops.SideEffectMutating)
	}

	cfg.Skills.AllowScripts = true
	if got := e.sideEffectCeiling(); got != ops.SideEffectRisky {
		t.Errorf("ceiling = %q with scripts on, want %q — run_script declares risky",
			got, ops.SideEffectRisky)
	}
}
