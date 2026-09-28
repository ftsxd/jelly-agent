package execution

import (
	"context"
	"errors"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

func TestApprovalForbiddenVerbPositions(t *testing.T) {
	for _, command := range []string{
		"kubectl -n app get secrets",
		"kubectl --namespace=app describe secrets",
		"kubectl -n app config view",
		"tccli --region ap-guangzhou cls DeleteTopic --TopicId demo",
		"tccli cls --region ap-guangzhou DeleteTopic --TopicId demo",
		"tccli --region=ap-guangzhou cls DeleteTopic --TopicId demo",
		"tccli --region=ap-guangzhou cvm TerminateInstances --InstanceIds x",
	} {
		ev := Evaluate(command, nil)
		t.Logf("%s => %s (%s)", command, ev.Decision, ev.Reason)
		if ev.Decision != Forbidden {
			t.Errorf("expected forbidden: %s", command)
		}
	}
}

func TestApprovalFaultConsumption(t *testing.T) {
	if !sandbox.OSSandboxAvailable() {
		t.Skip("OS sandbox required")
	}
	for _, scenario := range []string{"start-error", "timeout", "cancel-error"} {
		t.Run(scenario, func(t *testing.T) {
			store := approvalStore(t)
			cfg := approvalConfig()
			req := approvalRequest()
			a := newApproval(t, store, cfg, req)
			if err := store.Resolve(t.Context(), a.ID, "s", "admin", true, cfg); err != nil {
				t.Fatal(err)
			}
			calls := 0
			rt := Runtime{Config: cfg, Approvals: &store, run: func(context.Context, sandbox.Policy, sandbox.Spec) (sandbox.Result, error) {
				calls++
				switch scenario {
				case "start-error":
					return sandbox.Result{ExitCode: -1}, errors.New("synthetic start failure")
				case "cancel-error":
					return sandbox.Result{ExitCode: -1}, context.Canceled
				default:
					return sandbox.Result{ExitCode: -1, TimedOut: true, Backend: "os"}, nil
				}
			}}
			ctx := WithApproval(t.Context(), a.ID)
			out := rt.ExecuteApproved(ctx, "ops", "s", "call", req)
			got, _ := store.Get(t.Context(), a.ID)
			t.Logf("%s state=%s outcome=%s executed=%t error=%s", scenario, got.State, got.Outcome, out.Executed, out.Error)
			if got.State != "consumed" || got.Outcome != "failed" || got.ExecID == "" || calls != 1 {
				t.Fatal(got, calls, out)
			}
			rt.ExecuteApproved(ctx, "ops", "s", "call", req)
			if calls != 1 {
				t.Fatal("failure was replayed")
			}
		})
	}
}
func TestConsumedCrashRemainsNonReplayable(t *testing.T) {
	store := approvalStore(t)
	cfg := approvalConfig()
	req := approvalRequest()
	a := newApproval(t, store, cfg, req)
	if err := store.Resolve(t.Context(), a.ID, "s", "admin", true, cfg); err != nil {
		t.Fatal(err)
	}
	ctx := WithApproval(t.Context(), a.ID)
	if _, err := store.consume(ctx, cfg, "ops", "s", "call", req); err != nil {
		t.Fatal(err)
	}
	// Simulate loss of process between consumption and Finish.
	got, _ := store.Get(t.Context(), a.ID)
	if got.State != "consumed" || got.Outcome != "" {
		t.Fatal(got)
	}
	if _, err := store.consume(ctx, cfg, "ops", "s", "call", req); err == nil {
		t.Fatal("crash replayed")
	}
	t.Log("crash retained consumed/unknown outcome; no retry authority")
}
func TestRevocationAfterGrantBeforeConsumption(t *testing.T) {
	store := approvalStore(t)
	cfg := approvalConfig()
	req := approvalRequest()
	a := newApproval(t, store, cfg, req)
	if err := store.Resolve(t.Context(), a.ID, "s", "admin", true, cfg); err != nil {
		t.Fatal(err)
	}
	ctx := WithConfigGuard(WithApproval(t.Context(), a.ID), func(check func(Config) error) error { return check(Config{}) })
	if _, err := store.consume(ctx, cfg, "ops", "s", "call", req); err == nil {
		t.Fatal("revoked grant consumed")
	}
	store.Abandon(a.ID)
	got, _ := store.Get(t.Context(), a.ID)
	if got.State != "abandoned" {
		t.Fatal(got)
	}
}
