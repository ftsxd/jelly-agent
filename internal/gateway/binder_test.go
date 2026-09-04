package gateway

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// fakeToolset stands in for an MCP toolset: it hands back its tools per call,
// which is the property that forced a toolset wrapper rather than a tool one.
type fakeToolset struct {
	name  string
	tools []adktool.Tool
	calls int
}

// Name reports a constant, as ADK's mcptoolset does for every instance
// (tool/mcptoolset/set.go:96). The earlier fake returned its own name and so
// hid the bug this models: binding by set.Name() gave every server one
// executor slot.
func (s *fakeToolset) Name() string { return "mcp_tool_set" }
func (s *fakeToolset) Tools(agent.ReadonlyContext) ([]adktool.Tool, error) {
	s.calls++
	return s.tools, nil
}

func emptyReg() keyedReg {
	return keyedReg{names: fakeReg{}, keys: map[string]ops.ToolMetadata{}}
}

func governing(gw *Gateway, reg keyedReg) *Binder {
	return &Binder{
		GW:       gw,
		Registry: reg,
		Context:  func(agent.ToolContext) *ops.IncidentContext { return prodContext() },
		Fallback: Fallback{Govern: true, Timeout: 5 * time.Second, MaxResultBytes: 8000},
	}
}

// An MCP server's tools are the ones nobody has described, so if undeclared
// tools are not governed the toolset wrapper achieves nothing in practice.
func TestUndeclaredToolIsGovernedAndRuns(t *testing.T) {
	reg := emptyReg()
	inner := &innerTool{
		name:   "get_pods",
		decl:   &genai.FunctionDeclaration{Name: "get_pods", Description: "list pods"},
		result: map[string]any{"pods": []any{"a"}},
	}
	gw := New(Config{Registry: reg, Policy: Policy{MaxSideEffect: ops.SideEffectMutating}})

	tools := governing(gw, reg).Tools("k8s", []adktool.Tool{inner})
	w, ok := tools[0].(*Wrapped)
	if !ok {
		t.Fatalf("undeclared tool was not wrapped: %T", tools[0])
	}
	if w.Name() != "get_pods" {
		t.Errorf("name = %q; an uncontested name must survive — a mangled one costs selection accuracy", w.Name())
	}
	// The point of governing it: the call actually reaches the tool, and the
	// gateway has a record of it.
	if _, err := w.Run(newToolCtx(), map[string]any{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if inner.gotArgs == nil {
		t.Error("the inner tool was never reached")
	}
}

// Two servers exposing the same verb, neither described, is the ordinary case
// once a team runs more than one MCP server. Sharing one canonical name would
// send half the calls to the wrong cluster while looking like it worked.
func TestUndeclaredCollisionGetsDistinctNames(t *testing.T) {
	reg := emptyReg()
	gw := New(Config{Registry: reg})
	b := governing(gw, reg)

	decl := &genai.FunctionDeclaration{Name: "get_pods"}
	a := b.Tools("k8s-a", []adktool.Tool{&innerTool{name: "get_pods", decl: decl}})
	c := b.Tools("k8s-b", []adktool.Tool{&innerTool{name: "get_pods", decl: decl}})

	wa, ok := a[0].(*Wrapped)
	if !ok {
		t.Fatalf("first server's tool was not wrapped: %T", a[0])
	}
	wc, ok := c[0].(*Wrapped)
	if !ok {
		t.Fatalf("second server's tool was not wrapped (%T) — a name clash must be resolved, not surrendered to", c[0])
	}
	first, second := wa.Name(), wc.Name()
	if first == second {
		t.Fatalf("both servers got %q; the calls are no longer distinguishable", first)
	}
	if first != "get_pods" {
		t.Errorf("first = %q, want the uncontested name kept", first)
	}
	if second != "k8s-b__get_pods" {
		t.Errorf("second = %q, want it prefixed with its server", second)
	}
	// And each still routes home: the canonical name changed, the remote one
	// did not.
	if got := wc.meta.Remote(); got != "get_pods" {
		t.Errorf("remote name = %q, want the server's own name — renaming must not break routing", got)
	}
}

// A toolset is re-bound every turn. A name that drifted between turns would
// invalidate the model's own memory of what it just called.
func TestAdoptedNamesAreStableAcrossTurns(t *testing.T) {
	reg := emptyReg()
	gw := New(Config{Registry: reg})
	b := governing(gw, reg)
	set := &fakeToolset{name: "k8s", tools: []adktool.Tool{
		&innerTool{name: "get_pods", decl: &genai.FunctionDeclaration{Name: "get_pods"}},
	}}
	bound := b.Toolset("k8s", set)

	var names []string
	for turn := 0; turn < 3; turn++ {
		got, err := bound.Tools(nil)
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		w, ok := got[0].(*Wrapped)
		if !ok {
			t.Fatalf("turn %d: toolset tool not wrapped: %T", turn, got[0])
		}
		names = append(names, w.Name())
	}
	if names[0] != names[1] || names[1] != names[2] {
		t.Errorf("names across turns = %v, want one stable name", names)
	}
	if set.calls != 3 {
		t.Errorf("inner toolset consulted %d times, want 3 — the list must stay per-turn, not cached", set.calls)
	}
}

// A third-party server's silence is not a safety guarantee. This is the
// property that makes governing MCP worth doing rather than a formality.
func TestUndeclaredRemoteToolIsAssumedMutating(t *testing.T) {
	reg := emptyReg()
	gw := New(Config{Registry: reg, Policy: Policy{MaxSideEffect: ops.SideEffectReadOnly}})
	tools := governing(gw, reg).Tools("k8s", []adktool.Tool{
		&innerTool{name: "delete_pod", decl: &genai.FunctionDeclaration{Name: "delete_pod"}},
	})

	_, err := tools[0].(*Wrapped).Run(newToolCtx(), map[string]any{})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("err = %v, want ErrNotPermitted — an undeclared remote tool must not be assumed read-only", err)
	}
	if !strings.Contains(err.Error(), string(ops.SideEffectMutating)) {
		t.Errorf("err = %q, want it to name the level it assumed", err)
	}
}

// The bounds exist because zero in ToolMetadata means "no bound", which would
// put a 2MB pod list straight into the context window.
func TestFallbackSuppliesBounds(t *testing.T) {
	reg := emptyReg()
	gw := New(Config{Registry: reg})
	tools := governing(gw, reg).Tools("k8s", []adktool.Tool{
		&innerTool{name: "get_pods", decl: &genai.FunctionDeclaration{Name: "get_pods"}},
	})
	m := tools[0].(*Wrapped).meta
	if m.MaxResultBytes == 0 {
		t.Error("MaxResultBytes = 0, which means unbounded — a remote nobody sized must still be capped")
	}
	if m.Timeout == 0 {
		t.Error("Timeout = 0, which means no deadline — a hanging server would hang the turn")
	}
}

// Off is the previous behaviour, and it must stay reachable: a deployment that
// has not written metadata yet keeps its tools working on the direct path.
func TestFallbackOffLeavesUndeclaredToolsAlone(t *testing.T) {
	reg := emptyReg()
	gw := New(Config{Registry: reg})
	var reported []string
	b := &Binder{GW: gw, Registry: reg,
		Report: func(_ string, names []string) { reported = append(reported, names...) }}

	inner := &innerTool{name: "get_pods", decl: &genai.FunctionDeclaration{Name: "get_pods"}}
	got := b.Tools("k8s", []adktool.Tool{inner})
	if got[0] != adktool.Tool(inner) {
		t.Errorf("tool was wrapped with Govern off: %T", got[0])
	}
	if len(reported) != 1 || reported[0] != "k8s/get_pods" {
		t.Errorf("reported = %v, want the ungoverned tool named so the gap is visible", reported)
	}
}

// A declared entry must always win: adopting a tool cannot quietly redefine
// one someone configured.
func TestDeclaredMetadataBeatsAdoption(t *testing.T) {
	declared := ops.ToolMetadata{
		Name: "get_pods", RemoteName: "get_pods", Server: "k8s",
		SideEffect: ops.SideEffectReadOnly, Timeout: time.Second,
	}
	reg := keyedReg{
		names: fakeReg{declared.Name: declared},
		keys:  map[string]ops.ToolMetadata{declared.Key(): declared},
	}
	gw := New(Config{Registry: reg, Policy: Policy{MaxSideEffect: ops.SideEffectReadOnly}})
	tools := governing(gw, reg).Tools("k8s", []adktool.Tool{
		&innerTool{name: "get_pods", decl: &genai.FunctionDeclaration{Name: "get_pods"}},
	})

	m := tools[0].(*Wrapped).meta
	if m.SideEffect != ops.SideEffectReadOnly {
		t.Errorf("side effect = %q, want the declared read_only rather than the assumed level", m.SideEffect)
	}
	if m.Timeout != time.Second {
		t.Errorf("timeout = %v, want the declared one rather than the fallback", m.Timeout)
	}
}

// The gateway must not resolve a name it never saw on a real tool list, or a
// model that invents "restart_everything" gets it routed somewhere.
func TestAdoptionDoesNotAnswerForNamesNeverSeen(t *testing.T) {
	reg := emptyReg()
	gw := New(Config{Registry: reg})
	governing(gw, reg).Tools("k8s", []adktool.Tool{
		&innerTool{name: "get_pods", decl: &genai.FunctionDeclaration{Name: "get_pods"}},
	})

	if _, ok := gw.lookup("restart_everything"); ok {
		t.Fatal("the gateway resolved a tool nobody exposed")
	}
	res, err := gw.Execute(t.Context(), prodContext(), ops.OriginModel, "restart_everything", nil)
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
	if res.Call.ErrKind != "unknown_tool" {
		t.Errorf("err kind = %q; a routing failure must not count against a tool's success rate", res.Call.ErrKind)
	}
}


// The name a toolset reports is not the server it is. ADK's mcptoolset answers
// "mcp_tool_set" for every instance, so binding by it put two servers in one
// executor slot: the second registration took over the first server's traffic,
// and their duplicate tool names read as the same tool being re-bound rather
// than as a clash. Nothing errored — the calls just went to the wrong cluster.
//
// Only a run against two real MCP servers surfaced this; the unit fake was
// more cooperative than the real thing.
func TestServerIdentityComesFromConfigNotFromTheToolset(t *testing.T) {
	reg := emptyReg()
	gw := New(Config{Registry: reg, Policy: Policy{MaxSideEffect: ops.SideEffectMutating}})
	b := governing(gw, reg)

	mk := func(label string) adktool.Toolset {
		return &fakeToolset{tools: []adktool.Tool{&innerTool{
			name:   "get_pods",
			decl:   &genai.FunctionDeclaration{Name: "get_pods"},
			result: map[string]any{"from": label},
		}}}
	}
	a, err := b.Toolset("k8s-a", mk("a")).Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Toolset("k8s-b", mk("b")).Tools(nil)
	if err != nil {
		t.Fatal(err)
	}

	wa, ok := a[0].(*Wrapped)
	if !ok {
		t.Fatalf("k8s-a tool not wrapped: %T", a[0])
	}
	wc, ok := c[0].(*Wrapped)
	if !ok {
		t.Fatalf("k8s-b tool not wrapped: %T", c[0])
	}
	if wa.meta.Server == wc.meta.Server {
		t.Fatalf("both bound under server %q — the toolset's own name is not an identity", wa.meta.Server)
	}
	if wa.Name() == wc.Name() {
		t.Errorf("both exposed as %q; the clash went unnoticed", wa.Name())
	}

	// The decisive check: each call must reach its own server. Registering the
	// second executor under a shared key is what silently rerouted the first.
	got, err := wa.Run(newToolCtx(), map[string]any{})
	if err != nil {
		t.Fatalf("k8s-a run: %v", err)
	}
	if from := payloadField(t, got, "from"); from != "a" {
		t.Errorf("k8s-a call reached server %q — its traffic was taken over", from)
	}
}

// payloadField digs one field out of whatever shape the gateway returns, so
// the assertion above is about routing rather than about result shaping.
func payloadField(t *testing.T, res map[string]any, key string) any {
	t.Helper()
	if v, ok := res[key]; ok {
		return v
	}
	for _, v := range res {
		if m, ok := v.(map[string]any); ok {
			if inner, ok := m[key]; ok {
				return inner
			}
		}
	}
	t.Fatalf("no %q in %v", key, res)
	return nil
}
