package execution

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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

// AllowUnconfinedWithApproval must never let a call skip the sandbox on its
// own — it only turns "no sandbox mechanism available" into a Prompt that a
// human then has to approve, exactly like a WriteApproval escalation.
func TestUnconfinedEscapeHatchRequiresApprovalAndRecordsDegradation(t *testing.T) {
	restore := checkStrict
	checkStrict = func(sandbox.Policy) error { return fmt.Errorf("no sandbox on this host") }
	t.Cleanup(func() { checkStrict = restore })
	s := approvalStore(t)
	c := testConfig()
	req := Request{Command: "kubectl get pods", Purpose: "diagnose", Profile: "read"}
	calls := 0
	run := func(_ context.Context, p sandbox.Policy, _ sandbox.Spec) (sandbox.Result, error) {
		calls++
		if p.Strict || p.Backend != "native" {
			t.Fatalf("expected explicit unconfined native policy, got %+v", p)
		}
		return sandbox.Result{ExitCode: 0, Backend: "native"}, nil
	}

	// Default off: the flag's absence keeps failing closed exactly as before.
	r := Runtime{Config: c, Approvals: &s, run: run}
	if check := r.Check("ops", req); check.Decision != Allow {
		t.Fatalf("unset flag must not change the decision: %+v", check)
	}
	if out := r.Execute(t.Context(), "ops", "s", req); out.Executed || out.Error == "" || calls != 0 {
		t.Fatalf("unset flag must refuse without a sandbox: %+v", out)
	}

	c.Profiles[0].AllowUnconfinedWithApproval = true
	r.Config = c
	if check := r.Check("ops", req); check.Decision != Prompt || check.Reason == "" {
		t.Fatalf("expected sandbox-unavailable escalation to prompt, got %+v", check)
	}
	if out := r.Execute(t.Context(), "ops", "s", req); out.Executed || !out.ApprovalRequired || calls != 0 {
		t.Fatalf("unapproved call must not run unconfined: %+v", out)
	}
	// Forbidden stays forbidden; the escape hatch never rescues it.
	if check := r.Check("ops", Request{Command: "kubectl delete pod x", Purpose: "diagnose", Profile: "read"}); check.Decision != Forbidden {
		t.Fatalf("forbidden command escalated: %+v", check)
	}

	a := newApproval(t, s, c, req)
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, c); err != nil {
		t.Fatal(err)
	}
	out := r.ExecuteApproved(WithApproval(t.Context(), a.ID), "ops", "s", "call", req)
	if !out.Executed || !out.Unconfined || out.Degraded == "" || calls != 1 {
		t.Fatalf("expected an approved unconfined run with degradation recorded: %+v", out)
	}
}

// Approving a read to run unconfined is not a write grant: only a command
// that itself required approval may receive the profile's write sources.
func TestUnconfinedReadApprovalKeepsReadCredentials(t *testing.T) {
	restore := checkStrict
	checkStrict = func(sandbox.Policy) error { return fmt.Errorf("no sandbox on this host") }
	t.Cleanup(func() { checkStrict = restore })
	s := approvalStore(t)
	c := approvalConfig()
	c.Profiles[0].AllowUnconfinedWithApproval = true
	c.Profiles[0].Env = map[string]string{"TOKEN": "TEST_UNCONFINED_RO"}
	c.Profiles[0].WriteEnv = map[string]string{"TOKEN": "TEST_UNCONFINED_RW"}
	t.Setenv("TEST_UNCONFINED_RO", "readonly-credential")
	t.Setenv("TEST_UNCONFINED_RW", "write-credential")
	var got []string
	r := Runtime{Config: c, Approvals: &s, run: func(_ context.Context, _ sandbox.Policy, spec sandbox.Spec) (sandbox.Result, error) {
		got = append(got, spec.Env["TOKEN"])
		return sandbox.Result{ExitCode: 0, Backend: "native", Started: true}, nil
	}}
	for i, test := range []struct {
		command, want string
		write         bool
	}{
		{"kubectl get pods", "readonly-credential", false},
		{"kubectl scale deployment/web -n app --replicas=2", "write-credential", true},
	} {
		req := Request{Command: test.command, Purpose: "diagnose", Profile: "read"}
		if check := r.Check("ops", req); check.Decision != Prompt {
			t.Fatalf("%s: expected prompt, got %+v", test.command, check)
		}
		call := fmt.Sprintf("call-%d", i)
		a, err := s.Create(WithOrigin(t.Context(), "coordinator", "test"), c, "ops", "s", "round", call, req)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, c); err != nil {
			t.Fatal(err)
		}
		out := r.ExecuteApproved(WithApproval(t.Context(), a.ID), "ops", "s", call, req)
		if !out.Executed || !out.Unconfined || out.WriteCredentials != test.write || got[i] != test.want {
			t.Fatalf("%s: credential %q, observation %+v", test.command, got[i], out)
		}
	}
}

// A command that already needed approval must tell the approver it will run
// unconfined, and a grant must not survive the sandbox changing under it.
func TestUnconfinedRunsOnlyWhatTheApproverWasShown(t *testing.T) {
	restore := checkStrict
	t.Cleanup(func() { checkStrict = restore })
	sandboxOK := true
	checkStrict = func(sandbox.Policy) error {
		if sandboxOK {
			return nil
		}
		return fmt.Errorf("no sandbox on this host")
	}
	s := approvalStore(t)
	c := approvalConfig()
	c.Profiles[0].AllowUnconfinedWithApproval = true
	calls := 0
	r := Runtime{Config: c, Approvals: &s, run: func(context.Context, sandbox.Policy, sandbox.Spec) (sandbox.Result, error) {
		calls++
		return sandbox.Result{Started: true}, nil
	}}
	req := approvalRequest() // kubectl scale: needs approval on its own

	// Approved while sandboxed, then the sandbox breaks: the grant is void.
	a := newApproval(t, s, c, req)
	if strings.Contains(a.Reason, "无隔离") {
		t.Fatalf("sandboxed approval mentions unconfined: %q", a.Reason)
	}
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, c); err != nil {
		t.Fatal(err)
	}
	sandboxOK = false
	if out := r.ExecuteApproved(WithApproval(t.Context(), a.ID), "ops", "s", "call", req); out.Executed || calls != 0 {
		t.Fatalf("approval given for a sandboxed run executed unconfined: %+v", out)
	}

	// Asked while unconfined: the approver is told, and the run is unconfined.
	b, err := s.Create(WithOrigin(t.Context(), "coordinator", "test"), c, "ops", "s", "round", "call-2", req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.Reason, "获批后将以无隔离方式执行") {
		t.Fatalf("approver not told the run is unconfined: %q", b.Reason)
	}
	if err := s.Resolve(t.Context(), b.ID, "s", "admin", true, c); err != nil {
		t.Fatal(err)
	}
	out := r.ExecuteApproved(WithApproval(t.Context(), b.ID), "ops", "s", "call-2", req)
	if !out.Executed || !out.Unconfined || calls != 1 {
		t.Fatalf("expected the disclosed unconfined run: %+v", out)
	}

	// And the reverse: approved for an unconfined run, isolation comes back.
	d, err := s.Create(WithOrigin(t.Context(), "coordinator", "test"), c, "ops", "s", "round", "call-3", req)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Resolve(t.Context(), d.ID, "s", "admin", true, c); err != nil {
		t.Fatal(err)
	}
	sandboxOK = true
	if out := r.ExecuteApproved(WithApproval(t.Context(), d.ID), "ops", "s", "call-3", req); out.Executed || calls != 1 {
		t.Fatalf("grant for an unconfined run survived a sandbox change: %+v", out)
	}
}
