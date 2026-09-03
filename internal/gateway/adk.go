package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/adk/agent"
	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// toolCtxKey carries the ADK ToolContext through the gateway.
//
// A context value rather than a type assertion on ctx itself: the gateway
// wraps ctx with the tool's declared timeout, and the wrapped value is no
// longer an agent.ToolContext. An assertion would therefore fail for exactly
// the tools that bother to declare a timeout.
type toolCtxKey struct{}

// WithToolContext attaches the ADK ToolContext so an executor can recover it
// after the gateway has wrapped the context.
func WithToolContext(ctx context.Context, tc agent.ToolContext) context.Context {
	return context.WithValue(ctx, toolCtxKey{}, tc)
}

func toolContextFrom(ctx context.Context) (agent.ToolContext, bool) {
	// The value is checked first: it survives wrapping, while the assertion
	// only works on an unwrapped context.
	if tc, ok := ctx.Value(toolCtxKey{}).(agent.ToolContext); ok {
		return tc, true
	}
	tc, ok := ctx.(agent.ToolContext)
	return tc, ok
}

// ContextFunc supplies the incident a call belongs to.
//
// It takes the tool context because that is what identifies the conversation:
// one agent is built once and serves many incidents, so an incident captured
// at build time would be the wrong one from the second request onward.
type ContextFunc func(agent.ToolContext) *ops.IncidentContext

// KeyedRegistry resolves metadata by where a tool lives. Wrapping needs this
// rather than a name lookup: the name an ADK tool reports is its remote one,
// which is not unique across servers.
type KeyedRegistry interface {
	Registry
	ByKey(key string) (ops.ToolMetadata, bool)
}

// WrapTools routes one server's ADK tools through the gateway. server is the
// MCP server they came from, empty for built-ins.
//
// Only tools the registry knows are wrapped. An unregistered tool is returned
// untouched rather than dropped: dropping it would make a working tool vanish
// with nothing to explain why, and passing it through leaves today's behaviour
// intact for anything metadata has not reached yet. The count of unwrapped
// tools is returned so a caller can report it once at startup instead of
// leaving it invisible.
//
// A tool ADK cannot call anyway (no Declaration, no Run) is also passed
// through — wrapping it would claim a schema slot for something the model
// could never invoke.
func WrapTools(gw *Gateway, reg KeyedRegistry, server string, ic ContextFunc, tools []adktool.Tool) (wrapped []adktool.Tool, unwrapped []string) {
	out := make([]adktool.Tool, 0, len(tools))
	for _, t := range tools {
		r, ok := t.(runnable)
		if !ok {
			out = append(out, t)
			continue
		}
		// Looked up by (server, remote) because an ADK tool carries only the
		// name its own server gave it, and "get_pods" means nothing without
		// knowing which server said it — the very ambiguity the registry
		// exists to resolve.
		m, known := reg.ByKey(server + "/" + t.Name())
		if !known {
			out = append(out, t)
			unwrapped = append(unwrapped, server+"/"+t.Name())
			continue
		}
		out = append(out, &Wrapped{
			meta:        m,
			gw:          gw,
			context:     ic,
			origin:      ops.OriginModel,
			declaration: stripInjected(r.Declaration(), m),
			inner:       r,
		})
	}
	return out, unwrapped
}

// InnerExecutor turns one server's ADK tools into the executor the gateway
// calls.
//
// The indirection is what keeps the gateway free of ADK: it asks for a remote
// name and a map, and this closure finds the matching tool and calls it the
// way ADK expects. Keyed by the tools' own names, because that is exactly what
// the gateway hands back after translating ours.
func InnerExecutor(tools []adktool.Tool) Executor {
	byRemote := make(map[string]runnable, len(tools))
	for _, t := range tools {
		if r, ok := t.(runnable); ok {
			byRemote[t.Name()] = r
		}
	}
	return ExecutorFunc(func(ctx context.Context, remote string, args map[string]any) (map[string]any, error) {
		t, ok := byRemote[remote]
		if !ok {
			return nil, fmt.Errorf("gateway: 没有找到远端工具 %q", remote)
		}
		// The ToolContext travels as a context value, not by asserting the
		// type of ctx. The gateway wraps ctx with a timeout before calling
		// here, and a wrapped context is no longer an agent.ToolContext — an
		// assertion would fail for every tool that declares a timeout, which
		// is every tool worth declaring one for.
		tc, ok := toolContextFrom(ctx)
		if !ok {
			return nil, fmt.Errorf("gateway: 执行 %q 缺少 ADK 的 ToolContext", remote)
		}
		// The tool receives the original ToolContext, not the deadline-bearing
		// one: agent.ToolContext has no WithContext (only InvocationContext
		// does), so the deadline cannot be handed down. The gateway therefore
		// enforces the timeout on its own side — see runWithTimeout — and a
		// tool that overruns is abandoned rather than interrupted.
		return t.Run(tc, args)
	})
}

// runnable is the shape ADK needs from a tool it can call: a name and
// description for the catalogue, a declaration for the model, and a Run.
//
// It is declared here rather than imported because ADK's own version lives in
// an internal package. Satisfying it structurally is what lets a wrapped tool
// stand in for the original everywhere ADK looks.
type runnable interface {
	adktool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx agent.ToolContext, args any) (map[string]any, error)
}

// Wrapped is an ADK tool routed through the gateway.
//
// Every ADK-visible surface is answered from our metadata rather than the
// inner tool's: the name the model sees, the schema it fills, and the entry
// this tool claims in the request. That is what makes a tool addressable under
// a name its own server never heard of — and what stops an injected parameter
// from appearing in a schema the model is free to fill.
//
// Run does not call the inner tool directly. It hands the call to the gateway,
// which admits it, injects the arguments, translates the name and shapes the
// result. The inner tool is reached through an executor closure, so the
// gateway needs no knowledge of ADK.
type Wrapped struct {
	meta ops.ToolMetadata
	gw   *Gateway
	// context supplies the incident being diagnosed. It is a function because
	// one agent is built once and serves many incidents; capturing a value
	// here would pin the first one forever.
	context func(agent.ToolContext) *ops.IncidentContext
	// origin marks who chose this call. A tool reached through ADK's tool
	// calling was chosen by the model, by definition.
	origin ops.Origin

	declaration *genai.FunctionDeclaration
	inner       runnable
}

// Name is our name, not the server's. Two servers exposing get_pods are
// distinguishable precisely because this differs from what Run sends onward.
func (w *Wrapped) Name() string { return w.meta.Name }

// Description is ours too. A third-party server rewords its own description
// without telling us, and selection quality — plus the token cost of every
// turn — should not move when it does.
func (w *Wrapped) Description() string {
	if w.meta.Description != "" {
		return w.meta.Description
	}
	return w.inner.Description()
}

func (w *Wrapped) IsLongRunning() bool { return w.inner.IsLongRunning() }

// Declaration is the schema the model fills, with injected parameters removed.
//
// Removing them is the point: a parameter the model can see is a parameter it
// will set, and a model-supplied cluster is exactly the confident mistake this
// design exists to remove. The gateway would discard the value anyway, but
// leaving it in the schema invites the model to reason about something it does
// not control.
func (w *Wrapped) Declaration() *genai.FunctionDeclaration { return w.declaration }

// Run routes the call through the gateway.
//
// The result the model sees is the shaped summary and payload, not the tool's
// raw return — reduction happens here, before the bytes reach the context
// window, because a truncating step further downstream would keep the wrong
// part.
func (w *Wrapped) Run(ctx agent.ToolContext, args any) (map[string]any, error) {
	modelArgs, err := toArgMap(args)
	if err != nil {
		return nil, err
	}

	var ic *ops.IncidentContext
	if w.context != nil {
		ic = w.context(ctx)
	}

	// The conversation identifiers come from the tool context, so the recorded
	// row can be attributed to a session and an invocation — without them a
	// row says what happened but not during what.
	meta := CallMeta{
		SessionID:    ctx.SessionID(),
		InvocationID: ctx.InvocationID(),
		Agent:        ctx.AgentName(),
		CallID:       ctx.FunctionCallID(),
	}
	res, err := w.gw.ExecuteAs(WithToolContext(ctx, ctx), meta, ic, w.origin, w.meta.Name, modelArgs)
	if err != nil {
		// Returned as an error, not as a payload: ADK's after-tool callback
		// receives it either way, and an error keeps the failure legible to
		// the model instead of hiding it inside a successful-looking result.
		return nil, err
	}
	return toolPayload(res.Evidence), nil
}

// ProcessRequest packs this tool into the request under our name.
//
// ADK dispatches by looking up req.Tools[name] and calling Run on whatever it
// finds, so the entry has to be this wrapper. Delegating to the inner tool's
// ProcessRequest would register the remote name and the raw schema, and the
// gateway would never see the call.
func (w *Wrapped) ProcessRequest(ctx agent.ToolContext, req *adkmodel.LLMRequest) error {
	return packTool(req, w)
}

// packTool mirrors ADK's own toolutils.PackTool, which lives in an internal
// package we cannot import.
//
// The duplicate check stays: by the time a request is assembled the registry
// has already rejected clashing names, so this firing means something got
// past that — worth an error rather than a silent overwrite.
func packTool(req *adkmodel.LLMRequest, t runnable) error {
	if req.Tools == nil {
		req.Tools = make(map[string]any)
	}
	name := t.Name()
	if _, exists := req.Tools[name]; exists {
		return fmt.Errorf("gateway: 请求中已存在同名工具 %q", name)
	}
	req.Tools[name] = t

	decl := t.Declaration()
	if decl == nil {
		return nil
	}
	if req.Config == nil {
		req.Config = &genai.GenerateContentConfig{}
	}
	for _, existing := range req.Config.Tools {
		if existing != nil && existing.FunctionDeclarations != nil {
			existing.FunctionDeclarations = append(existing.FunctionDeclarations, decl)
			return nil
		}
	}
	req.Config.Tools = append(req.Config.Tools, &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{decl},
	})
	return nil
}

// toArgMap normalizes whatever ADK hands a tool into a map.
//
// Providers deliver arguments as a decoded map or as raw JSON depending on the
// transport, and a tool that accepted only one shape would work on one
// provider and fail on another.
func toArgMap(args any) (map[string]any, error) {
	switch a := args.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return a, nil
	case json.RawMessage:
		if len(a) == 0 {
			return nil, nil
		}
		var out map[string]any
		if err := json.Unmarshal(a, &out); err != nil {
			return nil, fmt.Errorf("gateway: 无法解析工具参数: %w", err)
		}
		return out, nil
	case []byte:
		if len(a) == 0 {
			return nil, nil
		}
		var out map[string]any
		if err := json.Unmarshal(a, &out); err != nil {
			return nil, fmt.Errorf("gateway: 无法解析工具参数: %w", err)
		}
		return out, nil
	case string:
		if a == "" {
			return nil, nil
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(a), &out); err != nil {
			return nil, fmt.Errorf("gateway: 无法解析工具参数: %w", err)
		}
		return out, nil
	default:
		// Round-trip anything else through JSON rather than reject it: a
		// provider-specific struct is still describable as an object.
		raw, err := json.Marshal(a)
		if err != nil {
			return nil, fmt.Errorf("gateway: 不支持的工具参数类型 %T", args)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("gateway: 不支持的工具参数类型 %T", args)
		}
		return out, nil
	}
}

// toolPayload renders evidence as the map ADK returns to the model.
//
// The evidence ID is included so the model can cite it — that citation is what
// Seal later checks, and a conclusion can only cite an ID it was shown.
func toolPayload(ev *ops.Evidence) map[string]any {
	if ev == nil {
		return map[string]any{}
	}
	out := map[string]any{
		"evidence_id": ev.ID,
		"summary":     ev.Summary,
	}
	if len(ev.Data) > 0 {
		var v any
		if err := json.Unmarshal(ev.Data, &v); err == nil {
			out["data"] = v
		}
	}
	if ev.Truncated {
		out["truncated"] = true
	}
	return out
}

// stripInjected returns a declaration without the host-supplied parameters.
func stripInjected(decl *genai.FunctionDeclaration, m ops.ToolMetadata) *genai.FunctionDeclaration {
	if decl == nil {
		return &genai.FunctionDeclaration{Name: m.Name, Description: m.Description}
	}
	out := *decl // shallow copy: ADK reuses the original for the inner tool
	out.Name = m.Name
	if m.Description != "" {
		out.Description = m.Description
	}

	if out.Parameters != nil {
		out.Parameters = stripSchema(out.Parameters, m)
	}
	if out.ParametersJsonSchema != nil {
		out.ParametersJsonSchema = stripJSONSchema(out.ParametersJsonSchema, m)
	}
	return &out
}

// stripSchema removes injected properties from a genai schema.
func stripSchema(s *genai.Schema, m ops.ToolMetadata) *genai.Schema {
	if s == nil || len(s.Properties) == 0 {
		return s
	}
	out := *s
	out.Properties = make(map[string]*genai.Schema, len(s.Properties))
	for name, prop := range s.Properties {
		if m.Injects(m.CanonicalArg(name)) {
			continue
		}
		out.Properties[name] = prop
	}
	// A required parameter the host fills must also leave the required list,
	// or a provider rejects the schema as unsatisfiable.
	if len(s.Required) > 0 {
		req := make([]string, 0, len(s.Required))
		for _, name := range s.Required {
			if m.Injects(m.CanonicalArg(name)) {
				continue
			}
			req = append(req, name)
		}
		out.Required = req
	}
	return &out
}

// stripJSONSchema removes injected properties from a raw JSON schema, which is
// how MCP tools carry their parameters.
func stripJSONSchema(schema any, m ops.ToolMetadata) any {
	raw, err := json.Marshal(schema)
	if err != nil {
		return schema
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return schema
	}

	if props, ok := doc["properties"].(map[string]any); ok {
		for name := range props {
			if m.Injects(m.CanonicalArg(name)) {
				delete(props, name)
			}
		}
	}
	if req, ok := doc["required"].([]any); ok {
		kept := make([]any, 0, len(req))
		for _, name := range req {
			if s, ok := name.(string); ok && m.Injects(m.CanonicalArg(s)) {
				continue
			}
			kept = append(kept, name)
		}
		doc["required"] = kept
	}
	return doc
}
