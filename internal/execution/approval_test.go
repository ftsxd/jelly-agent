package execution

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
	"github.com/jelly-agent/jelly-agent/internal/storage"
)

func approvalStore(t *testing.T) Approvals {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := EnsureApprovalSchema(db); err != nil {
		t.Fatal(err)
	}
	return Approvals{DB: db}
}
func approvalConfig() Config {
	c := testConfig()
	c.Profiles[0].WriteApproval = true
	c.Profiles[0].Network = true
	return c
}
func approvalRequest() Request {
	return Request{Command: "kubectl scale deployment/web -n app --replicas=2", Purpose: "恢复容量", Profile: "read"}
}
func newApproval(t *testing.T, s Approvals, c Config, req Request) Approval {
	t.Helper()
	a, err := s.Create(WithOrigin(t.Context(), "coordinator", "test"), c, "ops", "s", "round", "call", req)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestApprovalExactBindingExpiryAndSingleUse(t *testing.T) {
	s := approvalStore(t)
	c := approvalConfig()
	req := approvalRequest()
	a := newApproval(t, s, c, req)
	if err := s.Resolve(t.Context(), a.ID, "foreign", "admin", true, c); err == nil {
		t.Fatal("cross-session grant")
	}
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, c); err != nil {
		t.Fatal(err)
	}
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, c); err == nil {
		t.Fatal("duplicate grant")
	}
	if _, err := s.consume(t.Context(), c, "ops", "s", "call", req); err == nil {
		t.Fatal("missing server authority")
	}
	ctx := WithApproval(t.Context(), a.ID)
	changed := req
	changed.Command += " --timeout=5s"
	for _, trial := range []struct {
		agent, session, call string
		req                  Request
	}{
		{"other", "s", "call", req}, {"ops", "foreign", "call", req}, {"ops", "s", "other", req}, {"ops", "s", "call", changed},
	} {
		if _, err := s.consume(ctx, c, trial.agent, trial.session, trial.call, trial.req); err == nil {
			t.Fatal("changed binding consumed grant")
		}
	}
	if _, err := s.consume(ctx, c, "ops", "s", "call", req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.consume(ctx, c, "ops", "s", "call", req); err == nil {
		t.Fatal("replay consumed grant")
	}
	got, err := s.Get(t.Context(), a.ID)
	if err != nil || got.State != "consumed" || got.ResolvedBy != "admin" || got.Origin.Agent != "coordinator" {
		t.Fatal(got, err)
	}

	a = newApproval(t, s, c, req)
	s.DB.Exec(`UPDATE execution_approvals SET expires_ms=? WHERE id=?`, time.Now().Add(-time.Second).UnixMilli(), a.ID)
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, c); err == nil {
		t.Fatal("expired approval accepted")
	}
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", false, c); err != nil {
		t.Fatal("cannot reject expired request", err)
	}
	a = newApproval(t, s, c, req)
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, c); err != nil {
		t.Fatal(err)
	}
	s.DB.Exec(`UPDATE execution_approvals SET expires_ms=? WHERE id=?`, time.Now().Add(-time.Second).UnixMilli(), a.ID)
	if _, err := s.consume(WithApproval(t.Context(), a.ID), c, "ops", "s", "call", req); err == nil {
		t.Fatal("expired after approval still executed")
	}
}
func TestApprovalConfigRevocationAndRejection(t *testing.T) {
	s := approvalStore(t)
	c := approvalConfig()
	a := newApproval(t, s, c, approvalRequest())
	changed := c
	changed.TimeoutSec = 5
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, changed); err == nil {
		t.Fatal("changed config accepted")
	}
	list, err := s.List(t.Context(), "s", changed)
	if err != nil || list[0].State != "invalidated" {
		t.Fatal(list, err)
	}
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", false, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.consume(WithApproval(t.Context(), a.ID), c, "ops", "s", "call", approvalRequest()); err == nil {
		t.Fatal("rejected grant ran")
	}
	c.Profiles[0].WriteApproval = false
	if _, err := s.Create(t.Context(), c, "ops", "s", "r", "c", approvalRequest()); err == nil {
		t.Fatal("writes disabled")
	}
	c.Profiles[0].WriteApproval = true
	for _, cmd := range []string{"kubectl delete pod x", "kubectl scale deployment/x --token=secret", "kubectl config set x y", "kubectl get pods"} {
		req := approvalRequest()
		req.Command = cmd
		if _, err := s.Create(t.Context(), c, "ops", "s", "r", "c", req); err == nil {
			t.Fatalf("created approval for %s", cmd)
		}
	}
}
func TestApprovalConcurrentGrantAndConsumption(t *testing.T) {
	s := approvalStore(t)
	c := approvalConfig()
	req := approvalRequest()
	a := newApproval(t, s, c, req)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Resolve(context.Background(), a.ID, "s", "admin", true, c) == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("grant count", wins.Load())
	}
	wins.Store(0)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.consume(WithApproval(context.Background(), a.ID), c, "ops", "s", "call", req); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("execution count", wins.Load())
	}
}
func TestApprovalSurvivesReopenWithoutReusableAuthority(t *testing.T) {
	s := approvalStore(t)
	c := approvalConfig()
	req := approvalRequest()
	a := newApproval(t, s, c, req)
	// Another handle simulates another process opening the same durable store.
	var path string
	if err := s.DB.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	reopened := Approvals{DB: db}
	if err := reopened.Resolve(t.Context(), a.ID, "s", "admin", true, c); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.consume(t.Context(), c, "ops", "s", "call", req); err == nil {
		t.Fatal("persistent approved row became executable by replay")
	}
	reopened.Abandon(a.ID)
	if _, err := reopened.consume(WithApproval(t.Context(), a.ID), c, "ops", "s", "call", req); err == nil {
		t.Fatal("abandoned grant ran")
	}
}

func TestApprovalCredentialRotationRevokesPendingGrant(t *testing.T) {
	s := approvalStore(t)
	c := approvalConfig()
	c.Profiles[0].WriteEnv = map[string]string{"TOKEN": "TEST_ROTATING_SOURCE"}
	t.Setenv("TEST_ROTATING_SOURCE", "identity-before-restart")
	a := newApproval(t, s, c, approvalRequest())
	t.Setenv("TEST_ROTATING_SOURCE", "different-identity-after-restart")
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, c); err == nil {
		t.Fatal("changed injected identity approved")
	}
	list, err := s.List(t.Context(), "s", c)
	if err != nil || list[0].State != "invalidated" {
		t.Fatal(list, err)
	}
}
func TestApprovedRuntimeUsesWriteCredentialsOnlyOnce(t *testing.T) {
	if !sandbox.OSSandboxAvailable() {
		t.Skip("OS sandbox required")
	}
	s := approvalStore(t)
	c := approvalConfig()
	req := approvalRequest()
	c.Profiles[0].Env = map[string]string{"TOKEN": "TEST_RO_TOKEN"}
	c.Profiles[0].WriteEnv = map[string]string{"TOKEN": "TEST_RW_TOKEN"}
	t.Setenv("TEST_RO_TOKEN", "readonly-credential")
	t.Setenv("TEST_RW_TOKEN", "write-credential")
	calls := 0
	r := Runtime{Config: c, Approvals: &s, run: func(ctx context.Context, p sandbox.Policy, spec sandbox.Spec) (sandbox.Result, error) {
		calls++
		expected := "readonly-credential"
		if spec.Argv[1] == "scale" {
			expected = "write-credential"
		}
		if spec.Env["TOKEN"] != expected || !p.Strict {
			t.Fatal("wrong credential/sandbox", spec.Env, p)
		}
		return sandbox.Result{ExitCode: 0, Stdout: spec.Env["TOKEN"]}, nil
	}}
	if out := r.Execute(t.Context(), "ops", "s", req); out.Executed || calls != 0 {
		t.Fatal(out)
	}
	read := req
	read.Command = "kubectl get pods"
	if out := r.Execute(t.Context(), "ops", "s", read); !out.Executed || out.Stdout == "readonly-credential" {
		t.Fatal(out)
	}
	a := newApproval(t, s, c, req)
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, c); err != nil {
		t.Fatal(err)
	}
	ctx := WithApproval(t.Context(), a.ID)
	out := r.ExecuteApproved(ctx, "ops", "s", "call", req)
	if !out.Executed || !out.Approved || out.ApprovalID != a.ID || out.Stdout == "write-credential" {
		t.Fatal(out)
	}
	if out := r.ExecuteApproved(ctx, "ops", "s", "call", req); out.Executed || calls != 2 {
		t.Fatal("replayed", out, calls)
	}
	got, _ := s.Get(t.Context(), a.ID)
	if got.Outcome != "succeeded" || got.ExecID != out.ExecID {
		t.Fatal(got)
	}
}
