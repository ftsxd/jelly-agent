package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/adk/agent"
	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// innerTool stands in for an ADK tool — a built-in or one from mcptoolset.
type innerTool struct {
	name    string
	desc    string
	decl    *genai.FunctionDeclaration
	gotArgs any
	result  map[string]any
	err     error
}

func (t *innerTool) Name() string                            { return t.name }
func (t *innerTool) Description() string                     { return t.desc }
func (t *innerTool) IsLongRunning() bool                     { return false }
func (t *innerTool) Declaration() *genai.FunctionDeclaration { return t.decl }
func (t *innerTool) Run(_ agent.ToolContext, args any) (map[string]any, error) {
	t.gotArgs = args
	return t.result, t.err
}

// keyedReg resolves both by name (what the gateway uses) and by
// (server, remote) key (what wrapping uses).
type keyedReg struct {
	names fakeReg
	keys  map[string]ops.ToolMetadata
}

func (k keyedReg) Lookup(name string) (ops.ToolMetadata, bool) { return k.names.Lookup(name) }
func (k keyedReg) ByKey(key string) (ops.ToolMetadata, bool) {
	m, ok := k.keys[key]
	return m, ok
}

// toolCtx is the minimum ToolContext the wrapper path touches.
type toolCtx struct{ agent.StrictContextMock }

func newToolCtx() *toolCtx {
	return &toolCtx{agent.StrictContextMock{Ctx: context.Background()}}
}

func k8sDecl() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "get_pods",
		Description: "Server's own wording, which may change without notice",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"cluster":   {Type: genai.TypeString},
				"namespace": {Type: genai.TypeString},
				"selector":  {Type: genai.TypeString},
				"start":     {Type: genai.TypeString},
				"end":       {Type: genai.TypeString},
			},
			Required: []string{"cluster", "namespace", "selector"},
		},
	}
}

func wrapOne(t *testing.T, m ops.ToolMetadata, inner *innerTool, p Policy) (*Wrapped, *Gateway) {
	t.Helper()
	reg := keyedReg{names: fakeReg{m.Name: m}, keys: map[string]ops.ToolMetadata{m.Key(): m}}
	for _, a := range m.Aliases {
		reg.names[a] = m
	}
	tools := []adktool.Tool{inner}
	gw := New(Config{
		Registry:  reg,
		Executors: map[string]Executor{m.Server: InnerExecutor(tools)},
		Policy:    p,
	})
	wrapped, unwrapped := WrapTools(gw, reg, m.Server, func(agent.ToolContext) *ops.IncidentContext {
		return prodContext()
	}, tools)
	if len(unwrapped) != 0 {
		t.Fatalf("a registered tool was left unwrapped: %v", unwrapped)
	}
	w, ok := wrapped[0].(*Wrapped)
	if !ok {
		t.Fatalf("tool was not wrapped: %T", wrapped[0])
	}
	return w, gw
}

// A parameter the model can see is a parameter it will set. Removing the
// injected ones from the schema is what stops it reasoning about a cluster it
// does not control.
func TestDeclarationHidesInjectedParameters(t *testing.T) {
	m := k8sMeta()
	m.Description = "列出 Pod 状态；不要用它查节点资源"
	inner := &innerTool{name: "get_pods", desc: "server wording", decl: k8sDecl()}
	w, _ := wrapOne(t, m, inner, Policy{})

	decl := w.Declaration()
	if decl.Name != "k8s_get_pods" {
		t.Errorf("declared name = %q, want ours", decl.Name)
	}
	if decl.Description != m.Description {
		t.Errorf("declaration carries the server's wording: %q", decl.Description)
	}
	for _, hidden := range []string{"cluster", "namespace", "start", "end"} {
		if _, present := decl.Parameters.Properties[hidden]; present {
			t.Errorf("injected parameter %q is visible to the model", hidden)
		}
	}
	if _, present := decl.Parameters.Properties["selector"]; !present {
		t.Error("selector was hidden; that one is the model's to fill")
	}
	// A required parameter the host fills must leave the required list too, or
	// a provider rejects the schema as unsatisfiable.
	for _, req := range decl.Parameters.Required {
		if req != "selector" {
			t.Errorf("required still lists %q, which the host fills", req)
		}
	}
	// The inner tool's own declaration must be untouched: ADK reuses it.
	if _, present := inner.decl.Parameters.Properties["cluster"]; !present {
		t.Error("the inner tool's declaration was mutated")
	}
}

// MCP tools carry their parameters as a raw JSON schema rather than a genai
// one, and that path has to strip the same fields.
func TestRawJSONSchemaAlsoHidesInjectedParameters(t *testing.T) {
	m := k8sMeta()
	inner := &innerTool{
		name: "get_pods",
		decl: &genai.FunctionDeclaration{
			Name: "get_pods",
			ParametersJsonSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cluster":  map[string]any{"type": "string"},
					"selector": map[string]any{"type": "string"},
					"start":    map[string]any{"type": "string"},
				},
				"required": []any{"cluster", "selector"},
			},
		},
	}
	w, _ := wrapOne(t, m, inner, Policy{})

	raw, err := json.Marshal(w.Declaration().ParametersJsonSchema)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	props := doc["properties"].(map[string]any)
	for _, hidden := range []string{"cluster", "start"} {
		if _, present := props[hidden]; present {
			t.Errorf("injected parameter %q is visible in the JSON schema", hidden)
		}
	}
	if _, present := props["selector"]; !present {
		t.Error("selector was hidden from the JSON schema")
	}
	if req := doc["required"].([]any); len(req) != 1 || req[0] != "selector" {
		t.Errorf("required = %v, want only selector", req)
	}
}

// End to end through ADK's calling convention: our name in, the server's name
// out, arguments injected on the way.
func TestWrappedRunGoesThroughTheGateway(t *testing.T) {
	m := k8sMeta()
	inner := &innerTool{
		name: "get_pods", decl: k8sDecl(),
		result: map[string]any{"summary": "3/6 CrashLoopBackOff"},
	}
	w, _ := wrapOne(t, m, inner, Policy{})

	out, err := w.Run(newToolCtx(), map[string]any{"label_selector": "app=payment"})
	if err != nil {
		t.Fatal(err)
	}

	got, ok := inner.gotArgs.(map[string]any)
	if !ok {
		t.Fatalf("inner tool received %T", inner.gotArgs)
	}
	if got["cluster"] != "prod-a" || got["namespace"] != "payment" {
		t.Errorf("handle fields not injected: %v", got)
	}
	if got["selector"] != "app=payment" {
		t.Errorf("argument alias not canonicalized: %v", got)
	}
	if _, ok := got["start"].(string); !ok {
		t.Errorf("incident window not injected: %v", got)
	}

	// The model is handed an evidence ID it can cite; Seal later checks that
	// citation, and a conclusion can only cite an ID it was shown.
	if id, _ := out["evidence_id"].(string); id == "" {
		t.Errorf("result carries no evidence_id: %v", out)
	}
	if s, _ := out["summary"].(string); !strings.Contains(s, "CrashLoop") {
		t.Errorf("summary lost: %v", out)
	}
}

// Policy applies through the wrapper, and a denied call must not reach the
// tool.
func TestWrappedRunEnforcesPolicy(t *testing.T) {
	m := k8sMeta()
	m.SideEffect = ops.SideEffectMutating
	inner := &innerTool{name: "get_pods", decl: k8sDecl(), result: map[string]any{"summary": "ok"}}
	w, _ := wrapOne(t, m, inner, Policy{}) // read-only

	if _, err := w.Run(newToolCtx(), nil); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("err = %v, want ErrNotPermitted", err)
	}
	if inner.gotArgs != nil {
		t.Error("a denied call reached the tool")
	}
}

// Providers deliver arguments as a map or as raw JSON depending on transport;
// a tool that accepted only one shape would work on one provider and fail on
// another.
func TestArgumentsArriveInSeveralShapes(t *testing.T) {
	for name, in := range map[string]any{
		"map":        map[string]any{"selector": "app=payment"},
		"rawJSON":    json.RawMessage(`{"selector":"app=payment"}`),
		"bytes":      []byte(`{"selector":"app=payment"}`),
		"string":     `{"selector":"app=payment"}`,
		"nil":        nil,
		"emptyBytes": []byte(nil),
	} {
		t.Run(name, func(t *testing.T) {
			inner := &innerTool{name: "get_pods", decl: k8sDecl(), result: map[string]any{"summary": "ok"}}
			w, _ := wrapOne(t, k8sMeta(), inner, Policy{})
			if _, err := w.Run(newToolCtx(), in); err != nil {
				t.Fatalf("shape %s rejected: %v", name, err)
			}
			got := inner.gotArgs.(map[string]any)
			// Injection happens regardless of the incoming shape.
			if got["cluster"] != "prod-a" {
				t.Errorf("shape %s: injection did not happen: %v", name, got)
			}
		})
	}

	// Malformed JSON is an error, not a silent empty map: a swallowed parse
	// failure would run the tool with no arguments and look like a tool bug.
	inner := &innerTool{name: "get_pods", decl: k8sDecl()}
	w, _ := wrapOne(t, k8sMeta(), inner, Policy{})
	if _, err := w.Run(newToolCtx(), `{"selector":`); err == nil {
		t.Error("malformed arguments were accepted")
	}
}

// ADK dispatches by looking up req.Tools[name], so the entry has to be the
// wrapper under our name — otherwise the gateway never sees the call.
func TestProcessRequestRegistersOurNameAndSchema(t *testing.T) {
	m := k8sMeta()
	inner := &innerTool{name: "get_pods", decl: k8sDecl()}
	w, _ := wrapOne(t, m, inner, Policy{})

	req := &adkmodel.LLMRequest{}
	if err := w.ProcessRequest(newToolCtx(), req); err != nil {
		t.Fatal(err)
	}
	if _, ours := req.Tools["k8s_get_pods"]; !ours {
		t.Errorf("request registers %v, want our name", keysOf(req.Tools))
	}
	if _, theirs := req.Tools["get_pods"]; theirs {
		t.Error("the server's own name was registered; the gateway would be bypassed")
	}
	if got := req.Tools["k8s_get_pods"]; got != w {
		t.Error("the entry is not the wrapper, so Run would skip the gateway")
	}
	decls := req.Config.Tools[0].FunctionDeclarations
	if len(decls) != 1 || decls[0].Name != "k8s_get_pods" {
		t.Errorf("declarations = %+v", decls)
	}
	if _, present := decls[0].Parameters.Properties["cluster"]; present {
		t.Error("the model's schema still shows an injected parameter")
	}

	// A second pack under the same name is an error rather than a silent
	// overwrite: the registry should have caught it long before here.
	if err := w.ProcessRequest(newToolCtx(), req); err == nil {
		t.Error("packing the same name twice was accepted")
	}
}

// An unregistered tool keeps working. Dropping it would make a configured
// tool vanish with nothing to explain why; the count is returned so a caller
// can report it once instead of leaving it invisible.
func TestUnregisteredToolsPassThroughAndAreReported(t *testing.T) {
	known := k8sMeta()
	reg := keyedReg{names: fakeReg{known.Name: known}, keys: map[string]ops.ToolMetadata{known.Key(): known}}
	tools := []adktool.Tool{
		&innerTool{name: "get_pods", decl: k8sDecl()},
		&innerTool{name: "web_search", decl: &genai.FunctionDeclaration{Name: "web_search"}},
	}
	gw := New(Config{Registry: reg})

	wrapped, unwrapped := WrapTools(gw, reg, known.Server, nil, tools)
	if len(wrapped) != 2 {
		t.Fatalf("wrapped %d tools, want 2 — nothing may be dropped", len(wrapped))
	}
	if len(unwrapped) != 1 || !strings.HasSuffix(unwrapped[0], "web_search") {
		t.Errorf("unwrapped = %v, want the web_search entry reported", unwrapped)
	}
	if _, isWrapped := wrapped[1].(*Wrapped); isWrapped {
		t.Error("an unregistered tool was wrapped anyway")
	}
	if wrapped[1].Name() != "web_search" {
		t.Error("the pass-through tool changed name")
	}
}

// One agent serves many incidents, so the context has to be resolved per call
// rather than captured at build time.
func TestIncidentContextIsResolvedPerCall(t *testing.T) {
	m := k8sMeta()
	inner := &innerTool{name: "get_pods", decl: k8sDecl(), result: map[string]any{"summary": "ok"}}
	reg := keyedReg{names: fakeReg{m.Name: m}, keys: map[string]ops.ToolMetadata{m.Key(): m}}
	tools := []adktool.Tool{inner}
	gw := New(Config{
		Registry:  reg,
		Executors: map[string]Executor{m.Server: InnerExecutor(tools)},
	})

	cluster := "prod-a"
	wrapped, _ := WrapTools(gw, reg, m.Server, func(agent.ToolContext) *ops.IncidentContext {
		ic := prodContext()
		ic.Primary.Handles["kubernetes"].Ref["cluster"] = cluster
		return ic
	}, tools)
	w := wrapped[0].(*Wrapped)

	if _, err := w.Run(newToolCtx(), nil); err != nil {
		t.Fatal(err)
	}
	if inner.gotArgs.(map[string]any)["cluster"] != "prod-a" {
		t.Fatalf("first call: %v", inner.gotArgs)
	}

	// A later incident, same agent.
	cluster = "prod-b"
	if _, err := w.Run(newToolCtx(), nil); err != nil {
		t.Fatal(err)
	}
	if got := inner.gotArgs.(map[string]any)["cluster"]; got != "prod-b" {
		t.Errorf("second call used %q; the context was captured, not resolved", got)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
