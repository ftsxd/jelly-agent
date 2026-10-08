package execution

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
	"github.com/jelly-agent/jelly-agent/internal/storage"
)

func TestGrantClassIsNarrowAndRefusesWhatShouldStayPerCommand(t *testing.T) {
	for _, test := range []struct{ command, class string }{
		{"tccli cvm StopInstances --InstanceIds x", "tccli cvm StopInstances"},
		{"tccli cvm StartInstances --InstanceIds y", "tccli cvm StartInstances"},
		{"kubectl scale deployment/web -n app --replicas=2", "kubectl scale"},
		{"kubectl -n app rollout restart deployment/web", "kubectl rollout restart"},
		{"kubectl set image deployment/web web=nginx:1", "kubectl set image"},
		// Per command, always:
		{"tccli tke DescribeClusterKubeconfig --ClusterId c", ""}, // returns credentials
		{"kubectl exec web -- sh", ""},                            // arbitrary code
		{"kubectl apply -f https://example.com/x.yaml", ""},       // effect lives in a manifest
		{"kubectl port-forward svc/web 8080:80", ""},
		{"kubectl --unknown scale deployment/web", ""}, // subcommand not certain
		{"python3 -c 'print(1)'", ""},                  // "same kind" is any code
		{"curl https://example.com", ""},
		{"tccli configure list", ""}, // local credential files, not an API
		{"tccli cvm StopInstances --InstanceIds x && tccli cvm StartInstances", ""},
	} {
		if got := GrantClass(test.command); got != test.class {
			t.Errorf("%s: class %q, want %q", test.command, got, test.class)
		}
	}
}

func grantedRun(t *testing.T) (Approvals, Config, *int, Runtime) {
	t.Helper()
	s := approvalStore(t)
	c := approvalConfig()
	calls := 0
	r := Runtime{Config: c, Approvals: &s, run: func(context.Context, sandbox.Policy, sandbox.Spec) (sandbox.Result, error) {
		calls++
		return sandbox.Result{Started: true}, nil
	}}
	return s, c, &calls, r
}

func TestSessionGrantCoversItsClassOnly(t *testing.T) {
	s, c, calls, r := grantedRun(t)
	a := newApproval(t, s, c, approvalRequest()) // kubectl scale deployment/web …
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, c); err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGrant(t.Context(), c, a, "admin")
	if err != nil || g.Class != "kubectl scale" {
		t.Fatalf("grant %+v, %v", g, err)
	}

	other := Request{Command: "kubectl scale deployment/api -n app --replicas=3", Purpose: "扩容", Profile: "read"}
	found, ok := s.FindGrant(t.Context(), c, "ops", "s", other)
	if !ok || found.ID != g.ID {
		t.Fatal("same class not covered")
	}
	out := r.ExecuteGranted(t.Context(), "ops", "s", other, found)
	if !out.Executed || !out.Approved || out.ApprovalID != g.ID || out.Grant != "kubectl scale" || *calls != 1 {
		t.Fatalf("granted run %+v", out)
	}

	for name, miss := range map[string]func() (Grant, bool){
		"other class": func() (Grant, bool) {
			return s.FindGrant(t.Context(), c, "ops", "s", Request{Command: "kubectl rollout restart deployment/web", Profile: "read", Purpose: "p"})
		},
		"other session": func() (Grant, bool) { return s.FindGrant(t.Context(), c, "ops", "s2", other) },
		"other agent":   func() (Grant, bool) { return s.FindGrant(t.Context(), c, "ops2", "s", other) },
	} {
		if _, ok := miss(); ok {
			t.Errorf("%s: grant applied", name)
		}
	}

	changed := c
	changed.Profiles = append([]Profile(nil), c.Profiles...)
	changed.Profiles[0].Rules = []Rule{{Name: "new", Pattern: []string{"jq"}, Decision: Allow}}
	if _, ok := s.FindGrant(t.Context(), changed, "ops", "s", other); ok {
		t.Error("grant survived a configuration change")
	}

	if list, err := s.Grants(t.Context(), "s", c); err != nil || len(list) != 1 || list[0].Uses != 1 {
		t.Fatalf("grants %+v %v", list, err)
	}
	if err := s.RevokeGrant(t.Context(), "s", g.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.FindGrant(t.Context(), c, "ops", "s", other); ok {
		t.Fatal("revoked grant applied")
	}
}

func TestNoGrantForUnconfinedRuns(t *testing.T) {
	restore := checkStrict
	t.Cleanup(func() { checkStrict = restore })
	sandboxOK := true
	checkStrict = func(sandbox.Policy) error {
		if sandboxOK {
			return nil
		}
		return fmt.Errorf("no sandbox")
	}
	s, c, _, _ := grantedRun(t)
	c.Profiles[0].AllowUnconfinedWithApproval = true

	// Granted while sandboxed; the sandbox breaks: the grant no longer applies.
	a := newApproval(t, s, c, approvalRequest())
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGrant(t.Context(), c, a, "admin"); err != nil {
		t.Fatal(err)
	}
	sandboxOK = false
	if _, ok := s.FindGrant(t.Context(), c, "ops", "s", approvalRequest()); ok {
		t.Fatal("grant extended an unconfined run")
	}
	// And none is offered or made while the run would be unconfined.
	if class := c.GrantClassFor("ops", approvalRequest()); class != "" {
		t.Fatalf("offered %q for an unconfined run", class)
	}
	if _, err := s.CreateGrant(t.Context(), c, a, "admin"); err == nil {
		t.Fatal("grant made for an unconfined run")
	}
}

func TestUnmigratedStoreKeepsApprovalsWorking(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := storage.ApplySchema(db, approvalSchema, "execution_approvals"); err != nil {
		t.Fatal(err)
	}
	s := Approvals{DB: db}
	c := approvalConfig()
	a := newApproval(t, s, c, approvalRequest())
	if err := s.Resolve(t.Context(), a.ID, "s", "admin", true, c); err != nil {
		t.Fatal("approval broken without the grant table", err)
	}
	if _, err := s.Grants(t.Context(), "s", c); !errors.Is(err, ErrGrantUnavailable) {
		t.Fatalf("want ErrGrantUnavailable, got %v", err)
	}
	if _, err := s.CreateGrant(t.Context(), c, a, "admin"); !errors.Is(err, ErrGrantUnavailable) {
		t.Fatalf("want ErrGrantUnavailable, got %v", err)
	}
	if _, ok := s.FindGrant(t.Context(), c, "ops", "s", approvalRequest()); ok {
		t.Fatal("grant found without a table")
	}
}
