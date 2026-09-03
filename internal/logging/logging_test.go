package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func decode(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("record is not JSON: %v\n%s", err, line)
	}
	return m
}

func TestJSONIsTheDefaultAndCarriesService(t *testing.T) {
	var buf bytes.Buffer
	l := setupTo(&buf, Config{Service: "jelly-agent", Version: "test"})
	l.Info("配置已变更", "path", "/etc/jelly/config.yaml")

	m := decode(t, buf.Bytes())
	if m["msg"] != "配置已变更" {
		t.Errorf("msg = %v", m["msg"])
	}
	if m["service"] != "jelly-agent" || m["version"] != "test" {
		t.Errorf("service/version missing: %v", m)
	}
	// The value must be its own field, not embedded in the sentence — that is
	// the whole reason for the migration.
	if m["path"] != "/etc/jelly/config.yaml" {
		t.Errorf("path field missing: %v", m)
	}
}

// A slow span in Jaeger and the log lines from that turn have to be one thing
// to look at, which only works if the record carries the trace id.
func TestRecordsInsideASpanCarryTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	l := setupTo(&buf, Config{})

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	defer tp.Shutdown(context.Background())
	ctx, span := tp.Tracer("test").Start(context.Background(), "turn")
	l.InfoContext(ctx, "工具调用失败")
	span.End()

	m := decode(t, buf.Bytes())
	traceID, _ := m["trace_id"].(string)
	spanID, _ := m["span_id"].(string)
	if len(traceID) != 32 {
		t.Errorf("trace_id = %q, want a 32-char id", traceID)
	}
	if len(spanID) != 16 {
		t.Errorf("span_id = %q, want a 16-char id", spanID)
	}
	if traceID != span.SpanContext().TraceID().String() {
		t.Error("trace_id does not match the active span")
	}
}

// Logging without a context is not an error and must not invent ids.
func TestRecordsWithoutASpanHaveNoTraceFields(t *testing.T) {
	var buf bytes.Buffer
	l := setupTo(&buf, Config{})
	l.Info("启动完成")

	m := decode(t, buf.Bytes())
	if _, ok := m["trace_id"]; ok {
		t.Errorf("trace_id present without a span: %v", m)
	}
}

func TestLevelFiltersAndDefaultsToInfo(t *testing.T) {
	var buf bytes.Buffer
	l := setupTo(&buf, Config{Level: "warn"})
	l.Info("这条应被过滤")
	l.Warn("这条应保留")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d records, want 1: %q", len(lines), buf.String())
	}
	if m := decode(t, []byte(lines[0])); m["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", m["level"])
	}

	buf.Reset()
	setupTo(&buf, Config{Level: "口误"}).Info("未知级别应回落到 info")
	if buf.Len() == 0 {
		t.Error("an unknown level must fall back to info, not silence the logger")
	}
}

// Several dependencies log through the standard library. Leaving that stream
// un-redirected would mean one process emitting two different formats, and the
// JSON consumer choking on the other one.
func TestStandardLibraryLogIsRedirected(t *testing.T) {
	var buf bytes.Buffer
	setupTo(&buf, Config{})
	log.Printf("来自依赖库的一行")

	m := decode(t, buf.Bytes())
	if !strings.Contains(m["msg"].(string), "来自依赖库的一行") {
		t.Errorf("standard log output did not reach the handler: %v", m)
	}
}

func TestTextFormatForLocalWork(t *testing.T) {
	var buf bytes.Buffer
	setupTo(&buf, Config{Format: FormatText}).Info("人读的一行", "task", "digest")
	out := buf.String()
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("text format produced JSON: %q", out)
	}
	if !strings.Contains(out, "task=digest") {
		t.Errorf("text format lost the field: %q", out)
	}
}

func TestErrAttrUsesAStableKey(t *testing.T) {
	var buf bytes.Buffer
	setupTo(&buf, Config{}).Error("周期任务失败", Err(errors.New("dial tcp: refused")))
	if m := decode(t, buf.Bytes()); m["err"] != "dial tcp: refused" {
		t.Errorf("err field = %v", m)
	}
	buf.Reset()
	setupTo(&buf, Config{}).Error("无错误对象", Err(nil)) // must not panic
	_ = slog.Default()
}
