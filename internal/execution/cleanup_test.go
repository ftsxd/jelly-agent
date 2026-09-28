package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

func cleanupRuntimeConfig(t *testing.T) Config {
	t.Helper()
	c := testConfig()
	c.Profiles[0].Network = true
	if err := sandbox.CheckStrict((Runtime{Config: c}).policy(c.Profiles[0], Request{})); err != nil {
		t.Skip(err)
	}
	return c
}

func TestRuntimePrivateDirCleanupOnStartFailure(t *testing.T) {
	c := cleanupRuntimeConfig(t)
	var dir string
	r := Runtime{Config: c, run: func(_ context.Context, p sandbox.Policy, s sandbox.Spec) (sandbox.Result, error) {
		dir = s.Dir
		return sandbox.Result{}, errors.New("start failed")
	}}
	out := r.Execute(t.Context(), "ops", "s", Request{Command: "kubectl get pods", Profile: "read", Purpose: "audit cleanup"})
	if out.Executed || dir == "" {
		t.Fatalf("setup %+v %s", out, dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("start failure retains workspace: %s %v", dir, err)
	}
}
func TestRuntimePrivateKubeconfigCleanupAfterRestrictiveMode(t *testing.T) {
	t.Setenv("AUDIT_INLINE_KUBE", "apiVersion: v1\nclusters: []\nusers: [{name: audit, user: {token: audit-private-token}}]\n")
	c := cleanupRuntimeConfig(t)
	c.Profiles[0].KubeconfigEnv = "AUDIT_INLINE_KUBE"
	var dir string
	r := Runtime{Config: c, run: func(_ context.Context, p sandbox.Policy, s sandbox.Spec) (sandbox.Result, error) {
		dir = s.Dir
		if err := os.Chmod(filepath.Join(dir, ".kube"), 0); err != nil {
			t.Fatal(err)
		}
		return sandbox.Result{ExitCode: 0}, nil
	}}
	out := r.Execute(t.Context(), "ops", "s", Request{Command: "kubectl get pods", Profile: "read", Purpose: "audit cleanup"})
	if dir != "" {
		defer func() { os.Chmod(filepath.Join(dir, ".kube"), 0700); os.RemoveAll(dir) }()
	}
	if !out.Executed || dir == "" {
		t.Fatalf("setup %+v %s", out, dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("credential workspace remains: %s %v; out %+v", dir, err, out)
	}
}

func TestRuntimeStartedButWaitIncomplete(t *testing.T) {
	c := cleanupRuntimeConfig(t)
	r := Runtime{Config: c, run: func(_ context.Context, p sandbox.Policy, s sandbox.Spec) (sandbox.Result, error) {
		return sandbox.Result{Started: true, ExitCode: -1, Truncated: true}, errors.New("exec: WaitDelay expired before I/O complete")
	}}
	out := r.Execute(t.Context(), "ops", "s", Request{Command: "kubectl get pods", Profile: "read", Purpose: "audit final result"})
	if !out.Executed || !strings.HasPrefix(out.Error, "命令已启动") || !strings.Contains(out.Error, "请勿自动重试") {
		t.Fatalf("started classified incorrectly: %+v", out)
	}
}
