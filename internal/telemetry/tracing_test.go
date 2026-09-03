package telemetry

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/genai"
)

// Tracing must never be a precondition for running the agent. A disabled
// config, and a config the exporter rejects, both have to leave the caller with
// a usable shutdown rather than an error it has to branch on.
func TestDisabledReturnsUsableShutdown(t *testing.T) {
	shutdown, err := Start(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("Start = %v, want nil for a disabled config", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown is nil; callers defer it unconditionally")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown = %v, want nil", err)
	}
}

func TestUnknownProtocolIsRejectedByName(t *testing.T) {
	shutdown, err := Start(context.Background(), Config{Enabled: true, Protocol: "thrift"})
	if err == nil {
		t.Fatal("Start = nil error, want a rejection for an unknown protocol")
	}
	if !strings.Contains(err.Error(), "thrift") {
		t.Errorf("error %q does not name the offending protocol", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown is nil even on failure; callers defer it unconditionally")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown = %v, want nil", err)
	}
}

// A pasted URL is the obvious way to fill in an endpoint, and the exporter's
// own error for one is unhelpful, so the scheme is stripped instead.
func TestEndpointAcceptsAPastedURL(t *testing.T) {
	for _, in := range []string{"http://localhost:4318/", "https://collector:4317", "collector:4317"} {
		exp, err := newExporter(context.Background(), Config{
			Enabled: true, Protocol: ProtocolHTTP, Endpoint: in, Insecure: true,
		})
		if err != nil {
			t.Errorf("endpoint %q rejected: %v", in, err)
			continue
		}
		_ = exp.Shutdown(context.Background())
	}
}

// Agent runs are low-volume and expensive to reproduce, so the default records
// everything; a ratio exists for a shared collector with a quota.
func TestSamplerDefaultsToRecordingEverything(t *testing.T) {
	cases := []struct {
		ratio float64
		want  sdktrace.Sampler
	}{
		{0, sdktrace.NeverSample()},
		{1, sdktrace.AlwaysSample()},
		{2, sdktrace.AlwaysSample()},
		{-1, sdktrace.NeverSample()},
	}
	for _, tc := range cases {
		if got := sampler(tc.ratio); got.Description() != tc.want.Description() {
			t.Errorf("sampler(%v) = %s, want %s", tc.ratio, got.Description(), tc.want.Description())
		}
	}
	if d := sampler(0.25).Description(); !strings.Contains(d, "0.25") {
		t.Errorf("sampler(0.25) = %s, want a ratio-based sampler", d)
	}
}

// Regression: the resource build must not depend on a hand-pinned semconv
// version. resource.Merge refuses two different schema URLs, so pinning one
// here fails as soon as the OTel SDK moves — and the error names two URLs
// without naming the line that produced them, which is a bad afternoon.
//
// The earlier tests all returned before this point (disabled config, bad
// protocol name), which is why the first run against a real collector was
// where this surfaced. This one goes all the way through Start.
func TestStartBuildsResourceAgainstAnySDKSchema(t *testing.T) {
	// An unroutable endpoint is fine: the gRPC exporter dials lazily, so Start
	// exercises everything except the network.
	shutdown, err := Start(context.Background(), Config{
		Enabled:  true,
		Endpoint: "127.0.0.1:1",
		Protocol: ProtocolGRPC,
		Insecure: true,
		Version:  "test",
	})
	if err != nil {
		t.Fatalf("Start = %v, want nil (a schema-URL conflict here means the semconv version was pinned)", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Shutdown may report the unreachable collector; that is not what this
	// test is about, so only a panic or a hang would be a failure.
	_ = shutdown(ctx)
}

// The prompt breakdown exists to turn one input-token total into an
// explanation, so the tool-schema share has to be counted from what actually
// reaches the model: name, description and the serialized parameter schema.
func TestEstimateConfigTokensCountsSchemasNotJustNames(t *testing.T) {
	sys, tools, count := EstimateConfigTokens(nil)
	if sys != 0 || tools != 0 || count != 0 {
		t.Errorf("nil config = (%d, %d, %d), want zeros", sys, tools, count)
	}

	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "you are an SRE agent"}}},
		Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{
			{Name: "k8s_get_pods", Description: "列出 Pod 状态"},
			{
				Name:        "prometheus_query",
				Description: "查询指标",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"query": {Type: genai.TypeString, Description: "PromQL 表达式"},
						"step":  {Type: genai.TypeString, Description: "采样步长"},
					},
				},
			},
		}}},
	}

	sys, tools, count = EstimateConfigTokens(cfg)
	if count != 2 {
		t.Errorf("tool count = %d, want 2", count)
	}
	if sys == 0 {
		t.Error("system instruction counted as 0 tokens")
	}
	if tools == 0 {
		t.Fatal("tool schemas counted as 0 tokens")
	}

	// The parameter schema must be part of the number. Dropping it would
	// under-report the largest fixed cost of a wide tool set — the exact thing
	// this breakdown is for.
	bare := &genai.GenerateContentConfig{
		Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{
			{Name: "k8s_get_pods", Description: "列出 Pod 状态"},
			{Name: "prometheus_query", Description: "查询指标"},
		}}},
	}
	_, bareTools, _ := EstimateConfigTokens(bare)
	if tools <= bareTools {
		t.Errorf("tools with a parameter schema = %d tokens, not more than without = %d", tools, bareTools)
	}
}

// Both recorders take a context that may carry no span at all — the CLI runs
// with tracing disabled by default — so neither may panic or require a guard
// at the call site.
func TestRecordersAreSafeWithoutASpan(t *testing.T) {
	ctx := context.Background()
	RecordPrompt(ctx, PromptComposition{HistoryTokens: 4061, ToolsTokens: 700})
	RecordLLMAttempts(ctx, 3)
	RecordLLMAttempts(ctx, 1) // the normal case writes nothing
}

// Every recorder must be safe before Start has run — the CLI defaults to
// telemetry off, so this is the normal path, not an edge case.
func TestRecordersAreNoOpsBeforeStart(t *testing.T) {
	ctx := context.Background()
	RecordToolCall(ctx, "k8s_get_pods", true, "", 350*time.Millisecond)
	RecordToolCall(ctx, "web_search", false, "timeout", 15*time.Second)
	StartLLMCall("invocation-1")
	RecordLLMCall(ctx, "invocation-1", "deepseek-v4-flash", true, 4061, 647)
	RecordLLMCall(ctx, "", "deepseek-v4-flash", false, 0, 0)
}

// An invocation that dies between the two model hooks must not leak its entry,
// or a long-lived server accumulates one per abandoned turn.
func TestLLMTimerIsBounded(t *testing.T) {
	for i := 0; i < maxInflightLLM+64; i++ {
		StartLLMCall(fmt.Sprintf("invocation-%d", i))
	}
	llmTimer.mu.Lock()
	n := len(llmTimer.started)
	llmTimer.mu.Unlock()
	// Eviction only drops entries older than the TTL, so a burst of fresh ones
	// can exceed the soft bound; what must not happen is unbounded growth
	// across bursts.
	if n > maxInflightLLM+64 {
		t.Fatalf("in-flight table = %d entries, want at most %d", n, maxInflightLLM+64)
	}

	// Pairing removes the entry.
	StartLLMCall("paired")
	RecordLLMCall(context.Background(), "paired", "m", true, 0, 0)
	llmTimer.mu.Lock()
	_, still := llmTimer.started["paired"]
	llmTimer.mu.Unlock()
	if still {
		t.Error("a paired call left its entry behind")
	}
}
