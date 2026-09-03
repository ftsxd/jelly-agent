package telemetry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Metrics answer a different question from the spans next to them.
//
// A trace explains one run: this turn took six seconds and here is where they
// went. A metric explains the fleet over time: this tool's success rate has
// been sliding for a week, alert on it. Neither substitutes for the other, and
// a trace backend is the wrong place to ask the second question — traces are
// sampled and short-lived by design.
//
// ADK emits no metrics of its own (its telemetry setup still carries a
// "TODO init meter provider"), so everything here is ours. The data was
// already being computed for the SQLite tables and the span attributes; these
// instruments just publish a second copy to somewhere Prometheus can scrape.
const (
	meterName = "jelly-agent"

	// exportInterval is how often the reader pushes to the collector. Agent
	// runs are sparse — a minute of latency on a dashboard is invisible, while
	// a tighter loop just adds traffic for a process that is idle most of the
	// time.
	exportInterval = 30 * time.Second
)

// instruments is swapped in atomically at Setup so recording never has to lock
// or check a mutable global. A nil value means metrics are off, and every
// Record function below returns immediately — callers need no guard.
var instruments atomic.Pointer[meters]

type meters struct {
	toolCalls    metric.Int64Counter
	toolDuration metric.Float64Histogram

	llmCalls    metric.Int64Counter
	llmDuration metric.Float64Histogram
	llmTokens   metric.Int64Counter

	promptTokens metric.Int64Histogram
}

func newMeterProvider(ctx context.Context, cfg Config, res *resource.Resource) (*sdkmetric.MeterProvider, error) {
	endpoint := normalizeEndpoint(cfg.Endpoint)

	var reader sdkmetric.Reader
	switch strings.ToLower(cfg.Protocol) {
	case "", ProtocolGRPC:
		o := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(endpoint)}
		if cfg.Insecure {
			o = append(o, otlpmetricgrpc.WithInsecure())
		}
		exp, err := otlpmetricgrpc.New(ctx, o...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: otlp grpc metric exporter: %w", err)
		}
		reader = sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(exportInterval))
	case ProtocolHTTP:
		o := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(endpoint)}
		if cfg.Insecure {
			o = append(o, otlpmetrichttp.WithInsecure())
		}
		exp, err := otlpmetrichttp.New(ctx, o...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: otlp http metric exporter: %w", err)
		}
		reader = sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(exportInterval))
	default:
		return nil, fmt.Errorf("telemetry: unknown protocol %q (want %q or %q)", cfg.Protocol, ProtocolGRPC, ProtocolHTTP)
	}

	return sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	), nil
}

// buildInstruments creates every instrument once. An error here means a bad
// instrument definition, which is a programming mistake rather than a runtime
// condition, so it is returned rather than tolerated.
func buildInstruments(mp metric.MeterProvider) (*meters, error) {
	m := mp.Meter(meterName)
	var err error
	ms := &meters{}

	// Success rate lives on this counter rather than a ratio gauge: a ratio
	// computed in-process loses the counts, and the counts are what let a
	// dashboard say "3 of 40" instead of "92.5%" — the difference between a
	// real signal and one bad morning.
	if ms.toolCalls, err = m.Int64Counter("jelly.tool.calls",
		metric.WithDescription("工具调用次数，按工具、成败与失败归因拆分"),
		metric.WithUnit("{call}")); err != nil {
		return nil, err
	}
	if ms.toolDuration, err = m.Float64Histogram("jelly.tool.duration",
		metric.WithDescription("单次工具调用耗时"),
		metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if ms.llmCalls, err = m.Int64Counter("jelly.llm.calls",
		metric.WithDescription("模型调用次数，按模型与成败拆分"),
		metric.WithUnit("{call}")); err != nil {
		return nil, err
	}
	if ms.llmDuration, err = m.Float64Histogram("jelly.llm.duration",
		metric.WithDescription("单次模型调用耗时"),
		metric.WithUnit("s")); err != nil {
		return nil, err
	}
	// Tokens are a counter, not a histogram: the useful question is cumulative
	// spend over a window, and a rate over a counter answers it.
	if ms.llmTokens, err = m.Int64Counter("jelly.llm.tokens",
		metric.WithDescription("模型 token 消耗，按模型与输入/输出拆分"),
		metric.WithUnit("{token}")); err != nil {
		return nil, err
	}
	// Prompt composition is a histogram because the shape matters: the history
	// share creeping up is the thing to catch, and an average hides it.
	if ms.promptTokens, err = m.Int64Histogram("jelly.prompt.tokens",
		metric.WithDescription("每轮 prompt 的 token 构成，按部分拆分"),
		metric.WithUnit("{token}")); err != nil {
		return nil, err
	}
	return ms, nil
}

// Attribute keys shared by the instruments. Kept as constants because a
// dashboard query breaks silently on a typo.
const (
	attrKeyTool    = attribute.Key("tool")
	attrKeyOK      = attribute.Key("ok")
	attrKeyErrKind = attribute.Key("err_kind")
	attrKeyModel   = attribute.Key("model")
	attrKeyKind    = attribute.Key("kind")
	attrKeyPart    = attribute.Key("part")
)

// RecordToolCall publishes one tool invocation.
//
// errKind is recorded as an attribute only for failures. Tagging successes
// with an empty cause would double the series count for no information, and
// Prometheus charges for cardinality.
func RecordToolCall(ctx context.Context, tool string, ok bool, errKind string, dur time.Duration) {
	ms := instruments.Load()
	if ms == nil {
		return
	}
	attrs := []attribute.KeyValue{attrKeyTool.String(tool), attrKeyOK.Bool(ok)}
	if !ok {
		attrs = append(attrs, attrKeyErrKind.String(errKind))
	}
	set := metric.WithAttributes(attrs...)
	ms.toolCalls.Add(ctx, 1, set)
	ms.toolDuration.Record(ctx, dur.Seconds(), metric.WithAttributes(attrKeyTool.String(tool)))
}

// llmTimer pairs the before- and after-model hooks so a model call can report
// a duration.
//
// Keyed by invocation id, which is safe because ADK runs an invocation's model
// calls one after another — there is never a second call outstanding for the
// same invocation. ADK gives no per-call id for model calls the way it does
// for tools (FunctionCallID), so this is the available key.
//
// A missed pairing yields no duration rather than a wrong one: an unmatched
// Record simply skips the histogram.
var llmTimer = struct {
	mu      sync.Mutex
	started map[string]time.Time
}{started: make(map[string]time.Time)}

// maxInflightLLM bounds the table so an invocation that dies between the two
// hooks cannot leak an entry forever.
const maxInflightLLM = 256

// StartLLMCall notes that a model call is in flight.
func StartLLMCall(invocationID string) {
	if invocationID == "" {
		return
	}
	llmTimer.mu.Lock()
	defer llmTimer.mu.Unlock()
	if len(llmTimer.started) >= maxInflightLLM {
		cutoff := time.Now().Add(-10 * time.Minute)
		for k, v := range llmTimer.started {
			if v.Before(cutoff) {
				delete(llmTimer.started, k)
			}
		}
	}
	llmTimer.started[invocationID] = time.Now()
}

// RecordLLMCall publishes one model call, its latency and its token usage.
func RecordLLMCall(ctx context.Context, invocationID, model string, ok bool, inputTokens, outputTokens int64) {
	var dur time.Duration
	var timed bool
	if invocationID != "" {
		llmTimer.mu.Lock()
		if start, found := llmTimer.started[invocationID]; found {
			delete(llmTimer.started, invocationID)
			dur, timed = time.Since(start), true
		}
		llmTimer.mu.Unlock()
	}

	ms := instruments.Load()
	if ms == nil {
		return
	}
	ms.llmCalls.Add(ctx, 1, metric.WithAttributes(attrKeyModel.String(model), attrKeyOK.Bool(ok)))
	if timed {
		ms.llmDuration.Record(ctx, dur.Seconds(), metric.WithAttributes(attrKeyModel.String(model)))
	}
	if inputTokens > 0 {
		ms.llmTokens.Add(ctx, inputTokens,
			metric.WithAttributes(attrKeyModel.String(model), attrKeyKind.String("input")))
	}
	if outputTokens > 0 {
		ms.llmTokens.Add(ctx, outputTokens,
			metric.WithAttributes(attrKeyModel.String(model), attrKeyKind.String("output")))
	}
}

// recordPromptMetrics publishes the composition already computed for the span.
func recordPromptMetrics(ctx context.Context, c PromptComposition) {
	ms := instruments.Load()
	if ms == nil {
		return
	}
	for part, n := range map[string]int{
		"history": c.HistoryTokens,
		"system":  c.SystemTokens,
		"tools":   c.ToolsTokens,
	} {
		ms.promptTokens.Record(ctx, int64(n), metric.WithAttributes(attrKeyPart.String(part)))
	}
}
