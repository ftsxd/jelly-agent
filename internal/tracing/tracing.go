// Package tracing exports the agent's OpenTelemetry spans to an OTLP endpoint.
//
// There is no instrumentation code here, on purpose. ADK already traces the
// whole agent loop through the global TracerProvider — one span per agent
// invocation, one per model call (carrying gen_ai.usage.* token counts), one
// per tool execution — so installing a provider is the entire job. Adding our
// own spans around the same calls would duplicate what already exists and make
// the waterfall harder to read, not easier.
//
// This is separate from internal/metrics, and neither replaces the other.
// Traces answer "what happened in this one run, and where did the time go";
// they are typically sampled and kept for days. The metrics tables answer
// "what is the success rate of this tool over the last two months"; they are
// unsampled and kept indefinitely. A trace backend is the wrong place for the
// second question and a SQL table is a poor answer to the first.
package tracing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.36.0"
	adktelemetry "google.golang.org/adk/telemetry"
)

// Protocol selects the OTLP transport. Collectors normally listen for gRPC on
// 4317 and HTTP on 4318; Jaeger's all-in-one image accepts both.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http"
)

// Config is the tracing section of the app config.
type Config struct {
	Enabled     bool
	Endpoint    string  // host:port, no scheme
	Protocol    string  // grpc (default) | http
	ServiceName string  // defaults to jelly-agent
	Version     string  // reported as service.version
	SampleRatio float64 // 0 disables, 1 (default) records everything

	// Insecure sends plaintext OTLP. True is right for a collector on
	// localhost or inside a cluster; false requires the endpoint to serve TLS.
	Insecure bool

	// CaptureContent ships prompts and model replies.
	//
	// It is the single most useful setting for debugging an agent — "why did it
	// pick that tool" is answerable at a glance — and the single most dangerous
	// one to leave on in production: prompts carry whatever the incident
	// carried, and an observability backend rarely has the access controls a
	// database does. Default off; turn it on per environment, deliberately.
	//
	// Note where the content actually goes. ADK emits it through the OTel *logs*
	// API (gen_ai.system.message / gen_ai.choice records), not as span
	// attributes — payloads on spans would bloat every trace. So this flag also
	// decides whether a LoggerProvider is installed at all: with it off there is
	// no log pipeline, and nothing but traces leaves the process.
	//
	// The collector on the other end needs a logs pipeline. Without one it
	// rejects the logs signal and the exporter retries, which shows up as
	// repeated export errors on stderr.
	CaptureContent bool
}

const (
	defaultServiceName = "jelly-agent"
	defaultEndpoint    = "localhost:4317"
	exportTimeout      = 10 * time.Second
)

// Shutdown flushes pending spans and releases the exporter. Callers must run it
// before exit or the last run's spans — usually the interesting ones — never
// leave the process.
type Shutdown func(context.Context) error

// Start installs a global TracerProvider from cfg and returns its shutdown.
//
// A disabled config is not an error: it returns a no-op shutdown, so callers
// wire it unconditionally. Tracing is an aid, never a precondition for running
// the agent — an unreachable collector must not stop a diagnosis.
func Start(ctx context.Context, cfg Config) (Shutdown, error) {
	noop := func(context.Context) error { return nil }
	if !cfg.Enabled {
		return noop, nil
	}

	exp, err := newExporter(ctx, cfg)
	if err != nil {
		return noop, err
	}

	name := cfg.ServiceName
	if name == "" {
		name = defaultServiceName
	}
	// The schema URL comes from Default() rather than from the semconv package
	// this file imports. resource.Merge refuses to merge two resources with
	// different schema URLs, and Default()'s URL tracks the OTel SDK version —
	// so pinning a semconv version here breaks the moment anyone bumps the SDK,
	// with an error that names two URLs and not the line that caused it.
	// Attribute keys are stable across these versions; the URL is not.
	def := resource.Default()
	res, err := resource.Merge(def, resource.NewWithAttributes(
		def.SchemaURL(),
		semconv.ServiceNameKey.String(name),
		semconv.ServiceVersionKey.String(cfg.Version),
	))
	if err != nil {
		return noop, fmt.Errorf("tracing: build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithExportTimeout(exportTimeout)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler(cfg.SampleRatio)),
	)

	// Route through ADK's helper rather than calling otel.SetTracerProvider
	// directly: the content-capture flag lives behind its internal package, and
	// SetGlobalOtelProviders is the only exported way to set it.
	opts := []adktelemetry.Option{
		adktelemetry.WithTracerProvider(tp),
		adktelemetry.WithGenAICaptureMessageContent(cfg.CaptureContent),
	}

	var lp *sdklog.LoggerProvider
	if cfg.CaptureContent {
		lp, err = newLoggerProvider(ctx, cfg, res)
		if err != nil {
			_ = tp.Shutdown(ctx)
			return noop, err
		}
		opts = append(opts, adktelemetry.WithLoggerProvider(lp))
	}

	providers, err := adktelemetry.New(ctx, opts...)
	if err != nil {
		_ = tp.Shutdown(ctx)
		if lp != nil {
			_ = lp.Shutdown(ctx)
		}
		return noop, fmt.Errorf("tracing: init adk providers: %w", err)
	}
	providers.SetGlobalOtelProviders()

	return func(ctx context.Context) error { return providers.Shutdown(ctx) }, nil
}

// newLoggerProvider builds the pipeline that carries prompt and reply content.
//
// It shares the trace resource so both signals report the same service, which
// is what lets a backend line a log record up against the span it came from.
func newLoggerProvider(ctx context.Context, cfg Config, res *resource.Resource) (*sdklog.LoggerProvider, error) {
	endpoint := normalizeEndpoint(cfg.Endpoint)
	switch strings.ToLower(cfg.Protocol) {
	case "", ProtocolGRPC:
		o := []otlploggrpc.Option{otlploggrpc.WithEndpoint(endpoint)}
		if cfg.Insecure {
			o = append(o, otlploggrpc.WithInsecure())
		}
		e, err := otlploggrpc.New(ctx, o...)
		if err != nil {
			return nil, fmt.Errorf("tracing: otlp grpc log exporter: %w", err)
		}
		return sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(e)),
			sdklog.WithResource(res),
		), nil
	case ProtocolHTTP:
		o := []otlploghttp.Option{otlploghttp.WithEndpoint(endpoint)}
		if cfg.Insecure {
			o = append(o, otlploghttp.WithInsecure())
		}
		e, err := otlploghttp.New(ctx, o...)
		if err != nil {
			return nil, fmt.Errorf("tracing: otlp http log exporter: %w", err)
		}
		return sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(e)),
			sdklog.WithResource(res),
		), nil
	default:
		return nil, fmt.Errorf("tracing: unknown protocol %q (want %q or %q)", cfg.Protocol, ProtocolGRPC, ProtocolHTTP)
	}
}

// normalizeEndpoint drops a scheme and trailing slash. A pasted URL is the
// obvious mistake here and the exporter's own error for one is unhelpful.
func normalizeEndpoint(endpoint string) string {
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	return strings.TrimSuffix(endpoint, "/")
}

func newExporter(ctx context.Context, cfg Config) (*otlptrace.Exporter, error) {
	endpoint := normalizeEndpoint(cfg.Endpoint)

	switch strings.ToLower(cfg.Protocol) {
	case "", ProtocolGRPC:
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exp, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("tracing: otlp grpc exporter: %w", err)
		}
		return exp, nil
	case ProtocolHTTP:
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exp, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("tracing: otlp http exporter: %w", err)
		}
		return exp, nil
	default:
		return nil, fmt.Errorf("tracing: unknown protocol %q (want %q or %q)", cfg.Protocol, ProtocolGRPC, ProtocolHTTP)
	}
}

// sampler maps a ratio to a sampler, defaulting to always-on.
//
// Agent runs are low-volume and each one is expensive to reproduce, so the
// usual web-service instinct to sample at 1% is wrong here: the run you want to
// look at is the one that went badly, and there is no second chance to capture
// it. Ratios below 1 exist for a shared collector with a quota, not as a
// default.
func sampler(ratio float64) sdktrace.Sampler {
	switch {
	case ratio <= 0:
		return sdktrace.NeverSample()
	case ratio >= 1:
		return sdktrace.AlwaysSample()
	default:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	}
}
