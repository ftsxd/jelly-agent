package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeScript drops a script file into a fresh temp dir and returns (dir, name).
func writeScript(t *testing.T, name, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, name
}

func nativePolicy() Policy { return Policy{Backend: "native"} }

func TestNativeRunsAndInjectsEnv(t *testing.T) {
	dir, name := writeScript(t, "hi.sh", "#!/bin/sh\necho \"hi $WHO\"")
	res, err := Run(context.Background(), nativePolicy(), Spec{
		Dir: dir, Interp: "sh", RelFile: name, Env: map[string]string{"WHO": "jelly"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Backend != "native" {
		t.Fatalf("backend = %q", res.Backend)
	}
	if res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("unexpected result %+v", res)
	}
	if !strings.Contains(res.Output, "hi jelly") {
		t.Fatalf("env not injected: %q", res.Output)
	}
}

// The child must NOT inherit the host process environment — a secret in our own
// env should be invisible to the script.
func TestEnvScrubbed(t *testing.T) {
	t.Setenv("JELLY_SECRET_TOKEN", "leaked-value")
	dir, name := writeScript(t, "leak.sh", "#!/bin/sh\necho \"[$JELLY_SECRET_TOKEN]\"")
	res, err := Run(context.Background(), nativePolicy(), Spec{Dir: dir, Interp: "sh", RelFile: name})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(res.Output, "leaked-value") {
		t.Fatalf("host secret leaked into sandbox: %q", res.Output)
	}
	if !strings.Contains(res.Output, "[]") {
		t.Fatalf("expected empty value, got %q", res.Output)
	}
}

func TestOutputTruncated(t *testing.T) {
	dir, name := writeScript(t, "spam.sh", "#!/bin/sh\nyes x | head -c 100000")
	res, err := Run(context.Background(), Policy{Backend: "native", MaxOutput: 1024}, Spec{
		Dir: dir, Interp: "sh", RelFile: name,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Truncated {
		t.Fatal("expected truncation")
	}
	if len(res.Output) > 1024+len("\n…（输出已截断）") {
		t.Fatalf("output not capped: %d bytes", len(res.Output))
	}
}

func TestPolicyTimeout(t *testing.T) {
	dir, name := writeScript(t, "slow.sh", "#!/bin/sh\nsleep 5")
	start := time.Now()
	res, err := Run(context.Background(), Policy{Backend: "native", Timeout: 200 * time.Millisecond}, Spec{
		Dir: dir, Interp: "sh", RelFile: name,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected timeout, got %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("kill was slow: %s — process group may not be terminated", elapsed)
	}
}

func TestNonZeroExitIsNotAnError(t *testing.T) {
	dir, name := writeScript(t, "fail.sh", "#!/bin/sh\necho oops\nexit 3")
	res, err := Run(context.Background(), nativePolicy(), Spec{Dir: dir, Interp: "sh", RelFile: name})
	if err != nil {
		t.Fatalf("exit status should not be a Run error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestMissingInterpreterFails(t *testing.T) {
	dir, name := writeScript(t, "x.zz", "whatever")
	res, err := Run(context.Background(), nativePolicy(), Spec{
		Dir: dir, Interp: "definitely-not-a-real-interpreter-xyz", RelFile: name,
	})
	// Depending on whether the sh ulimit-wrapper is active, a missing
	// interpreter surfaces either as a start error or a non-zero exit. Either
	// way the run must not look successful.
	if err == nil && res.ExitCode == 0 {
		t.Fatalf("missing interpreter looked successful: %+v", res)
	}
}

func TestAuditHookFires(t *testing.T) {
	var got AuditEvent
	prev := Audit
	Audit = func(ev AuditEvent) { got = ev }
	defer func() { Audit = prev }()

	dir, name := writeScript(t, "ok.sh", "#!/bin/sh\nexit 0")
	if _, err := Run(context.Background(), nativePolicy(), Spec{Dir: dir, Interp: "sh", RelFile: name}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.Backend != "native" || got.File != name {
		t.Fatalf("audit event not populated: %+v", got)
	}
}

// docker auto-selection must degrade to native when no docker binary exists,
// rather than failing the run.
func TestDockerBackendFallsBackToNative(t *testing.T) {
	if dockerAvailable() {
		t.Skip("docker present; fallback path not exercised here")
	}
	dir, name := writeScript(t, "hi.sh", "#!/bin/sh\necho ok")
	res, err := Run(context.Background(), Policy{Backend: "docker"}, Spec{Dir: dir, Interp: "sh", RelFile: name})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Backend != "native" {
		t.Fatalf("expected fallback to native, got %q", res.Backend)
	}
}
