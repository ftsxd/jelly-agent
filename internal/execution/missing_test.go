package execution

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

// Fixtures name kubectl and tccli freely; a developer machine need not have
// them. Tests of the lookup itself restore the real resolver.
func TestMain(m *testing.M) {
	commandOnPath = func(string, string) bool { return true }
	os.Exit(m.Run())
}

func realLookup(t *testing.T) {
	t.Helper()
	stub := commandOnPath
	commandOnPath = realCommandOnPath
	t.Cleanup(func() { commandOnPath = stub })
}

func installFake(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMissingCommandRefusedBeforeApprovalOrStart(t *testing.T) {
	realLookup(t)
	bin := t.TempDir()
	installFake(t, bin, "kubectl", "bash")
	if err := os.WriteFile(filepath.Join(bin, "noexec"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	c := approvalConfig()
	s := approvalStore(t)
	r := Runtime{Config: c, Approvals: &s, run: func(context.Context, sandbox.Policy, sandbox.Spec) (sandbox.Result, error) {
		t.Fatal("missing command started")
		return sandbox.Result{}, nil
	}}
	for _, test := range []struct{ command, missing string }{
		{"tccli cls DescribeTopics", "tccli"},      // allow path
		{"find / -maxdepth 5 -name tccli", "find"}, // prompt path: no approval card
		{"noexec --version", "noexec"},             // present but not executable
		{"kubectl get pods | jq .items", "jq"},     // any segment
		{"kubectl get pods && echo done", ""},      // builtin inside bash
		{"kubectl get pods", ""},                   // present
	} {
		req := Request{Command: test.command, Purpose: "diagnose", Profile: "read"}
		if got := r.MissingCommand("ops", req); got != test.missing {
			t.Fatalf("%s: missing %q, want %q", test.command, got, test.missing)
		}
		if test.missing == "" {
			continue
		}
		out := r.Execute(t.Context(), "ops", "s", req)
		if out.Executed || out.ApprovalRequired || out.MissingCommand != test.missing || out.Error == "" {
			t.Fatalf("%s: %+v", test.command, out)
		}
	}
	// A single command is exec'd directly, so a builtin name is not runnable.
	if got := r.MissingCommand("ops", Request{Command: "echo hi", Profile: "read"}); got != "echo" {
		t.Fatalf("single-segment builtin: %q", got)
	}
	// Docker resolves inside the image; only the exit status can tell.
	r.Config.Backend = "docker"
	if got := r.MissingCommand("ops", Request{Command: "tccli help", Profile: "read"}); got != "" {
		t.Fatalf("docker precheck: %q", got)
	}
}

func TestMissingCommandSeesToolDir(t *testing.T) {
	realLookup(t)
	t.Setenv("PATH", t.TempDir())
	tools := t.TempDir()
	if err := os.Mkdir(filepath.Join(tools, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	installFake(t, filepath.Join(tools, "bin"), "tccli")
	c := testConfig()
	c.Profiles[0].ToolDir = tools
	r := Runtime{Config: c}
	if got := r.MissingCommand("ops", Request{Command: "tccli help", Profile: "read"}); got != "" {
		t.Fatalf("tool dir command reported missing: %q", got)
	}
}

func TestExitCode127TellsModelToStop(t *testing.T) {
	r := Runtime{Config: testConfig(), run: func(context.Context, sandbox.Policy, sandbox.Spec) (sandbox.Result, error) {
		return sandbox.Result{ExitCode: 127, Started: true, Stderr: "sh: tccli: not found"}, nil
	}}
	out := r.Execute(t.Context(), "ops", "s", Request{Command: "kubectl get pods", Purpose: "diagnose", Profile: "read"})
	if !out.Executed || out.ExitCode != 127 || out.Outcome != "failed" || out.Error == "" {
		t.Fatalf("%+v", out)
	}
}
