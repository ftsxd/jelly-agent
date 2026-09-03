package tracing

import (
	"context"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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
