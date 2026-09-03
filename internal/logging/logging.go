// Package logging installs the process-wide structured logger.
//
// Before this package the codebase logged three different ways — log.Printf
// with a bracketed prefix, fmt.Fprintf to stderr, and a per-package logf
// helper — none of which had levels, machine-readable fields, or any way to
// reach a log backend. A line like "周期任务 %q 失败: %v" is readable on a
// terminal and useless to a query: finding every failure of one task means
// parsing a sentence.
//
// JSON is the default because these logs are meant to be shipped. Message text
// stays human (and Chinese, as the rest of the operator-facing output does);
// field keys are stable ASCII so a query can rely on them.
//
// One thing this package deliberately does not swallow: output meant for the
// person at the terminal. The interactive shell's "[错误] …" lines and the
// CLI's final error before exit are program output, not telemetry — rendering
// them as JSON objects would make interactive use unreadable. They stay on
// stderr as they were.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// Format selects the encoding.
const (
	FormatJSON = "json"
	FormatText = "text"
)

// Config is the logging section of the app config.
type Config struct {
	// Level is debug | info | warn | error. Empty means info.
	Level string
	// Format is json (default) or text. Text exists for local work, where a
	// terminal is the destination and JSON costs more than it gives.
	Format string
	// AddSource records the file and line that emitted each record. Useful
	// while chasing a specific message, expensive enough not to leave on.
	AddSource bool
	// Service is reported on every record so a shipped log can be attributed
	// without relying on the collector to add it.
	Service string
	// Version is reported alongside Service.
	Version string
}

const defaultService = "jelly-agent"

// Setup installs the default slog logger and returns it.
//
// It also redirects the standard library's log package, so any call site not
// yet migrated — and any dependency that uses log.Printf, of which there are
// several — lands in the same stream with the same encoding instead of
// bypassing it.
func Setup(cfg Config) *slog.Logger {
	return setupTo(os.Stderr, cfg)
}

func setupTo(w io.Writer, cfg Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(cfg.Level), AddSource: cfg.AddSource}

	var base slog.Handler
	if strings.EqualFold(cfg.Format, FormatText) {
		base = slog.NewTextHandler(w, opts)
	} else {
		base = slog.NewJSONHandler(w, opts)
	}

	service := cfg.Service
	if service == "" {
		service = defaultService
	}
	attrs := []slog.Attr{slog.String("service", service)}
	if cfg.Version != "" {
		attrs = append(attrs, slog.String("version", cfg.Version))
	}

	logger := slog.New(&traceHandler{inner: base.WithAttrs(attrs)})
	slog.SetDefault(logger)

	// Dependencies (and any not-yet-migrated call site) write through the
	// standard logger; route it here so one process produces one stream.
	// Level info: a library's log.Printf carries no level of its own, and
	// discarding it would be worse than over-reporting it.
	slog.SetLogLoggerLevel(slog.LevelInfo)
	return logger
}

// traceHandler adds the active span's identifiers to every record.
//
// This is the point of structured logging here. With trace_id on the record, a
// slow span in Jaeger and the log lines from that same turn become one thing
// to look at; without it they are two haystacks that happen to share a clock.
//
// Only the *Context variants of the slog API carry a context, so a call site
// that logs without one simply gets no ids — no error, no guessing.
type traceHandler struct{ inner slog.Handler }

func (h *traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *traceHandler) Handle(ctx context.Context, rec slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		rec.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, rec)
}

func (h *traceHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &traceHandler{inner: h.inner.WithAttrs(as)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{inner: h.inner.WithGroup(name)}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Err wraps an error as an attribute under a stable key, so every failure in
// the codebase is queryable the same way regardless of who logged it.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.String("err", "")
	}
	return slog.String("err", err.Error())
}

// Fields renders a record the way the text handler would, for the few places
// that still need a formatted line (a legacy logf shim, a test helper).
func Fields(format string, args ...any) string { return fmt.Sprintf(format, args...) }
