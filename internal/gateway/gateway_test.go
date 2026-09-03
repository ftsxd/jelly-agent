package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/toolreg"
)

// fakeReg is a registry the test controls directly.
type fakeReg map[string]ops.ToolMetadata

func (f fakeReg) Lookup(name string) (ops.ToolMetadata, bool) {
	m, ok := f[name]
	return m, ok
}

// recorder captures what actually reached the executor — the remote name and
// the final arguments, which is where injection and translation are visible.
type recorder struct {
	remote string
	args   map[string]any
	result map[string]any
	err    error
	delay  time.Duration
}

func (r *recorder) Execute(ctx context.Context, remote string, args map[string]any) (map[string]any, error) {
	r.remote, r.args = remote, args
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.result, r.err
}

func k8sMeta() ops.ToolMetadata {
	return ops.ToolMetadata{
		Name: "k8s_get_pods", RemoteName: "get_pods", Server: "kubernetes-prod",
		Backend: "kubernetes", SideEffect: ops.SideEffectReadOnly,
		Produces:       ops.KindWorkloadStatus,
		InjectedParams: []string{"cluster", "namespace"},
		WindowParams:   [2]string{"start", "end"},
		ArgAliases:     map[string][]string{"selector": {"label_selector"}},
	}
}

func prodContext() *ops.IncidentContext {
	return &ops.IncidentContext{
		Window: ops.NewWindow(time.Now().Add(-2*time.Hour), ops.WindowFromAlert),
		Primary: &ops.Target{
			Canonical: "payment-gateway", Env: "prod",
			Handles: map[string]ops.Handle{"kubernetes": {
				Backend: "kubernetes",
				Ref:     map[string]string{"cluster": "prod-a", "namespace": "payment"},
			}},
		},
		Targets: []ops.Target{{Handles: map[string]ops.Handle{"kubernetes": {Backend: "kubernetes"}}}},
	}
}

func newGW(t *testing.T, m ops.ToolMetadata, exec Executor, p Policy) *Gateway {
	t.Helper()
	names := fakeReg{m.Name: m}
	for _, a := range m.Aliases {
		names[a] = m
	}
	return New(Config{
		Registry:  names,
		Executors: map[string]Executor{m.Server: exec},
		Policy:    p,
	})
}

// The point of injection: the model never sees cluster/namespace/window, so it
// cannot get them wrong. Anything it supplies for them is discarded, because a
// model-supplied cluster is exactly the confident mistake this removes.
func TestInjectedArgumentsComeFromContextNotTheModel(t *testing.T) {
	rec := &recorder{result: map[string]any{"summary": "6/6 Running"}}
	g := newGW(t, k8sMeta(), rec, Policy{})

	res, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "k8s_get_pods",
		map[string]any{
			"label_selector": "app=payment", // an alias
			"cluster":        "staging-b",   // the model guessing — must be ignored
			"start":          "whenever",    // ditto
		})
	if err != nil {
		t.Fatal(err)
	}

	if rec.args["cluster"] != "prod-a" || rec.args["namespace"] != "payment" {
		t.Errorf("handle fields not injected: %v", rec.args)
	}
	if rec.args["selector"] != "app=payment" {
		t.Errorf("argument alias not canonicalized: %v", rec.args)
	}
	if _, present := rec.args["label_selector"]; present {
		t.Error("the alias survived alongside the canonical name")
	}
	if rec.args["start"] == "whenever" {
		t.Error("the model's window value was used; context must be authoritative")
	}
	if _, ok := rec.args["start"].(string); !ok {
		t.Errorf("window not injected: %v", rec.args)
	}
	if res.Call.Args["cluster"] != "prod-a" {
		t.Error("the audit record does not show the arguments actually sent")
	}
}

// The naming split is what makes two servers exposing get_pods addressable:
// the model uses our name, the server is called with its own.
func TestOurNameIsTranslatedToTheRemoteName(t *testing.T) {
	rec := &recorder{result: map[string]any{"summary": "ok"}}
	g := newGW(t, k8sMeta(), rec, Policy{})

	if _, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "k8s_get_pods", nil); err != nil {
		t.Fatal(err)
	}
	if rec.remote != "get_pods" {
		t.Errorf("called the server with %q, want its own name get_pods", rec.remote)
	}
}

// Whichever alias the model used, statistics must attribute the call to one
// name — otherwise the same tool appears several times in every report.
func TestCallsViaAnAliasAreAttributedToTheCanonicalName(t *testing.T) {
	m := k8sMeta()
	m.Aliases = []string{"get_pods"}
	rec := &recorder{result: map[string]any{"summary": "ok"}}
	g := newGW(t, m, rec, Policy{})

	res, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "get_pods", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Call.Tool != "k8s_get_pods" {
		t.Errorf("Call.Tool = %q, want the canonical name", res.Call.Tool)
	}
	if res.Evidence.Source.Tool != "k8s_get_pods" {
		t.Errorf("Evidence cites %q, want the canonical name", res.Evidence.Source.Tool)
	}
}

// Provenance is stamped by the gateway, never by the tool: a tool must not be
// able to vouch for its own evidence.
func TestEvidenceCarriesProvenanceTheToolDidNotSupply(t *testing.T) {
	rec := &recorder{result: map[string]any{"summary": "3/6 CrashLoopBackOff"}}
	g := newGW(t, k8sMeta(), rec, Policy{})
	ic := prodContext()

	res, err := g.Execute(context.Background(), ic, ops.OriginWorkflow, "k8s_get_pods", nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := res.Evidence
	if ev == nil {
		t.Fatal("a successful call produced no evidence")
	}
	if ev.ID == "" {
		t.Error("evidence has no ID; a conclusion could not cite it")
	}
	if ev.Source.Server != "kubernetes-prod" || ev.Source.Backend != "kubernetes" {
		t.Errorf("Source = %+v", ev.Source)
	}
	if ev.Origin != ops.OriginWorkflow {
		t.Errorf("Origin = %q, want the caller's", ev.Origin)
	}
	if !ev.Pinned() {
		t.Error("workflow evidence should be pinned against eviction")
	}
	if ev.Window != ic.Window {
		t.Error("evidence does not record the window it covers")
	}
	if ev.Kind != ops.KindWorkloadStatus {
		t.Errorf("Kind = %q, want the tool's declared kind", ev.Kind)
	}
	if ev.ObservedAt.IsZero() {
		t.Error("ObservedAt not stamped")
	}
	if res.Call.EvidenceIDs[0] != ev.ID {
		t.Error("the audit record does not link to the evidence it produced")
	}
}

// Reduction, not truncation: the shaper turns a payload the model cannot read
// into the sentence it needs.
func TestShaperReducesRatherThanTruncates(t *testing.T) {
	var samples []map[string]any
	for i := 0; i < 720; i++ {
		samples = append(samples, map[string]any{"t": i, "v": float64(i) / 720})
	}
	rec := &recorder{result: map[string]any{"samples": samples}}

	m := k8sMeta()
	m.Name, m.Produces = "prom_query", ops.KindMetricSeries
	g := New(Config{
		Registry:  fakeReg{"prom_query": m},
		Executors: map[string]Executor{m.Server: rec},
		Shapers: map[string]Shaper{"prom_query": ShaperFunc(func(raw map[string]any, ev *ops.Evidence) error {
			n := len(raw["samples"].([]map[string]any))
			ev.Kind = ops.KindMetricSeries
			ev.Summary = "14:10 起单调上升，14:22 达 limit 的 98%"
			ev.Data = json.RawMessage(`{"points":` + string(rune('0'+n/100)) + `00,"peak":0.98}`)
			return nil
		})},
	})

	res, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "prom_query", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Evidence.Summary, "98%") {
		t.Errorf("Summary = %q, want the trend the model can act on", res.Evidence.Summary)
	}
	// The whole point: the shaped payload is a fraction of the raw one.
	if len(res.Evidence.Data) > 100 {
		t.Errorf("shaped data is %d bytes; reduction did not happen", len(res.Evidence.Data))
	}
	if res.Call.ResultBytes < 1000 {
		t.Errorf("ResultBytes = %d; the audit record should show the real payload size", res.Call.ResultBytes)
	}
}

// A tool reporting failure inside an otherwise-successful payload is still a
// failure. Missing this is how a success rate reads 100%.
func TestErrorInsideThePayloadIsAFailure(t *testing.T) {
	rec := &recorder{result: map[string]any{"error": "dial tcp 10.0.0.1:6443: connection refused"}}
	g := newGW(t, k8sMeta(), rec, Policy{})

	res, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "k8s_get_pods", nil)
	if err == nil {
		t.Fatal("a payload carrying an error was reported as success")
	}
	if res.Call.OK {
		t.Error("Call.OK = true")
	}
	if res.Call.ErrKind != "tool_error" {
		t.Errorf("ErrKind = %q", res.Call.ErrKind)
	}
	if res.Evidence != nil {
		t.Error("a failed call produced evidence; there is no observation to cite")
	}
}

// Every outcome produces an audit record, because a success rate computed over
// successes alone is not a success rate.
func TestEveryFailureStillProducesAnAuditRecord(t *testing.T) {
	cases := map[string]struct {
		exec    Executor
		wantErr string
	}{
		"transport": {&recorder{err: errors.New("boom")}, "upstream"},
		"timeout":   {&recorder{delay: 300 * time.Millisecond}, "timeout"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := k8sMeta()
			m.Timeout = 50 * time.Millisecond
			g := newGW(t, m, tc.exec, Policy{})

			res, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "k8s_get_pods", nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			if res.Call.OK || res.Call.ErrKind != tc.wantErr {
				t.Errorf("Call = %+v, want ok=false errKind=%q", res.Call, tc.wantErr)
			}
			if res.Call.Duration == 0 {
				t.Error("Duration not recorded")
			}
			if res.Call.Tool == "" {
				t.Error("the record does not say which tool failed")
			}
		})
	}
}

// A call that should not run must not appear in tool statistics as a tool
// problem — it is a routing or policy decision.
func TestUnknownToolIsARoutingFailureNotAToolFailure(t *testing.T) {
	g := newGW(t, k8sMeta(), &recorder{}, Policy{})
	res, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "k8s_delete_everything", nil)
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
	if res.Call.ErrKind != "unknown_tool" {
		t.Errorf("ErrKind = %q, want it distinguishable from a tool failure", res.Call.ErrKind)
	}
}

func TestPolicyRejectsEffectsBeyondWhatTheSessionAllows(t *testing.T) {
	restart := ops.ToolMetadata{
		Name: "k8s_restart", RemoteName: "restart", Server: "kubernetes-prod",
		Backend: "kubernetes", SideEffect: ops.SideEffectMutating,
	}
	rec := &recorder{result: map[string]any{"summary": "restarted"}}

	// Default policy is read-only, which is the safe default for a path that
	// has not thought about it.
	g := newGW(t, restart, rec, Policy{})
	res, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "k8s_restart", nil)
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("err = %v, want ErrNotPermitted", err)
	}
	if res.Call.ErrKind != "denied" {
		t.Errorf("ErrKind = %q", res.Call.ErrKind)
	}
	if rec.remote != "" {
		t.Error("a denied call reached the executor")
	}

	// A caller that allows mutation gets through.
	g = newGW(t, restart, rec, Policy{MaxSideEffect: ops.SideEffectMutating})
	if _, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "k8s_restart", nil); err != nil {
		t.Errorf("a permitted call was rejected: %v", err)
	}
}

// An MCP tool that declares no side effect is not assumed safe.
func TestUndeclaredRemoteToolIsNotAssumedSafe(t *testing.T) {
	unknown := ops.ToolMetadata{Name: "mystery", RemoteName: "mystery", Server: "third-party"}
	g := newGW(t, unknown, &recorder{result: map[string]any{}}, Policy{})
	if _, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "mystery", nil); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("err = %v, want the call rejected under a read-only policy", err)
	}

	// A built-in with nothing declared is read-only.
	builtin := ops.ToolMetadata{Name: "web_search"}
	g = New(Config{
		Registry:  fakeReg{"web_search": builtin},
		Executors: map[string]Executor{"": &recorder{result: map[string]any{"summary": "ok"}}},
	})
	if _, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "web_search", nil); err != nil {
		t.Errorf("a built-in was rejected: %v", err)
	}
}

func TestApprovalRequiredIsItsOwnOutcome(t *testing.T) {
	m := ops.ToolMetadata{
		Name: "mysql_kill", RemoteName: "kill", Server: "mysql",
		SideEffect: ops.SideEffectMutating, NeedsApproval: true,
		ApprovalReason: "会中断正在执行的会话",
	}
	g := newGW(t, m, &recorder{}, Policy{MaxSideEffect: ops.SideEffectMutating})

	_, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "mysql_kill", nil)
	if !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("err = %v, want ErrApprovalRequired", err)
	}
	if !strings.Contains(err.Error(), "会中断") {
		t.Errorf("error %q does not carry the reason a human needs", err)
	}

	g = newGW(t, m, &recorder{result: map[string]any{"summary": "killed"}},
		Policy{MaxSideEffect: ops.SideEffectMutating, AllowApprovalRequired: true})
	if _, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "mysql_kill", nil); err != nil {
		t.Errorf("a caller with its own approval flow was blocked: %v", err)
	}
}

// Evidence IDs appear in prompts and reports, so they must be short, unique
// and ordered.
func TestEvidenceIDsAreUniqueAndOrdered(t *testing.T) {
	g := newGW(t, k8sMeta(), &recorder{result: map[string]any{"summary": "ok"}}, Policy{})
	var ids []string
	for i := 0; i < 5; i++ {
		res, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "k8s_get_pods", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Evidence.ID)
		if len(res.Evidence.ID) > 5 {
			t.Errorf("ID %q is too long for a prompt", res.Evidence.ID)
		}
	}
	// Gapless: a reader following a citation chain reads a missing e3 as an
	// observation that went astray, so the sequence must not skip.
	want := []string{"e1", "e2", "e3", "e4", "e5"}
	for i, id := range ids {
		if id != want[i] {
			t.Fatalf("IDs = %v, want %v — a gap reads as a lost observation", ids, want)
		}
	}
}

// A shaper that fails must not lose an observation we already paid for.
func TestAFailedShaperKeepsTheRawObservation(t *testing.T) {
	m := k8sMeta()
	g := New(Config{
		Registry:  fakeReg{m.Name: m},
		Executors: map[string]Executor{m.Server: &recorder{result: map[string]any{"pods": "6/6"}}},
		Shapers: map[string]Shaper{m.Name: ShaperFunc(func(map[string]any, *ops.Evidence) error {
			return errors.New("shaper bug")
		})},
	})

	res, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "k8s_get_pods", nil)
	if err != nil {
		t.Fatalf("a shaper bug failed the call: %v", err)
	}
	if !strings.Contains(res.Evidence.Summary, "整形失败") {
		t.Errorf("Summary = %q, should say shaping failed", res.Evidence.Summary)
	}
	if !strings.Contains(string(res.Evidence.Data), "6/6") {
		t.Error("the raw observation was discarded")
	}
}

func TestByteCeilingTruncatesAndSaysSo(t *testing.T) {
	m := k8sMeta()
	m.MaxResultBytes = 40
	long := strings.Repeat("x", 500)
	g := newGW(t, m, &recorder{result: map[string]any{"content": long}}, Policy{})

	res, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "k8s_get_pods", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Evidence.Data) > 40 {
		t.Errorf("data is %d bytes, want at most 40", len(res.Evidence.Data))
	}
	if !res.Evidence.Truncated || !res.Call.Truncated {
		t.Error("truncation was not recorded; a reader would take the excerpt for the whole")
	}
}

// Better to route nothing than to route blindly.
func TestGatewayWithNoRegistryRejectsEverything(t *testing.T) {
	g := New(Config{})
	if _, err := g.Execute(context.Background(), nil, ops.OriginModel, "anything", nil); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("err = %v, want ErrUnknownTool", err)
	}
}

// A tool with no incident context still gets a usable window rather than a
// zero one, and the window is marked as guessed.
func TestMissingContextYieldsAGuessedWindowNotAZeroOne(t *testing.T) {
	rec := &recorder{result: map[string]any{"summary": "ok"}}
	g := newGW(t, k8sMeta(), rec, Policy{})

	res, err := g.Execute(context.Background(), nil, ops.OriginModel, "k8s_get_pods", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Evidence.Window.Until.IsZero() {
		t.Fatal("window is zero; a time-aware tool would query nothing")
	}
	if !res.Evidence.Window.Guessed() {
		t.Error("a window with nothing to anchor to must be marked guessed")
	}
}

// The gateway reads a real registry snapshot through the same interface.
func TestSnapshotAdaptsTheRealRegistry(t *testing.T) {
	r, conflicts := toolreg.Build([]ops.ToolMetadata{k8sMeta()})
	if len(conflicts) != 0 {
		t.Fatal(conflicts)
	}
	rec := &recorder{result: map[string]any{"summary": "ok"}}
	g := New(Config{
		Registry:  Snapshot(r),
		Executors: map[string]Executor{"kubernetes-prod": rec},
	})
	if _, err := g.Execute(context.Background(), prodContext(), ops.OriginModel, "k8s_get_pods", nil); err != nil {
		t.Fatal(err)
	}
	if rec.remote != "get_pods" {
		t.Errorf("remote = %q", rec.remote)
	}
}
