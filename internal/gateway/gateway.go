// Package gateway is the single path every tool call takes.
//
// It exists because five problems live at exactly this point, and solving them
// anywhere else means solving them twice — once for the deterministic workflow
// path and once for the model-chosen path, which then drift:
//
//	admission   a tool whose side effect is not permitted here never runs
//	arguments   cluster, namespace, instance and the incident window are
//	            injected from context, so the model cannot get them wrong —
//	            it never sees them
//	naming      our name is translated to the server's remote name, which is
//	            what makes two servers exposing get_pods addressable at all
//	shaping     a result is reduced, not truncated: 720 metric samples become
//	            one sentence about the trend, because truncation keeps the
//	            first fifth of the window — the part before anything happened
//	provenance  the observation is stamped into Evidence the conclusion can
//	            cite and a reviewer can check
//
// The order is fixed and not interchangeable. Admission comes first because a
// call that should not run should not appear in any statistic. Shaping and
// minting come last because provenance is only known here.
//
// This is a process-local layer, not a service. Every call passes through it,
// and it needs the request context (for the active span), the incident context
// (for arguments) and an in-process cache — none of which survive a network
// hop. "Tools as a service" already has a standard form, and it is MCP.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/toolreg"
)

// Executor runs one tool against the outside world. It is the seam the gateway
// wraps: a built-in function, or an MCP tool addressed by its remote name.
type Executor interface {
	// Execute calls the tool named remote with args. The gateway has already
	// resolved the name and filled injected arguments.
	Execute(ctx context.Context, remote string, args map[string]any) (map[string]any, error)
}

// ExecutorFunc adapts a function to Executor.
type ExecutorFunc func(ctx context.Context, remote string, args map[string]any) (map[string]any, error)

func (f ExecutorFunc) Execute(ctx context.Context, remote string, args map[string]any) (map[string]any, error) {
	return f(ctx, remote, args)
}

// Shaper reduces a raw tool result to what a model can use.
//
// Reduction, not truncation. A metric query returning 720 samples is 5,000
// tokens the model cannot read anyway; the sentence it needs is "rose steadily
// from 14:10, hit 98% of the limit at 14:22". Cutting the payload at a byte
// count would instead keep the first fifth of the window, which is the period
// before anything went wrong.
//
// A shaper fills Kind, Summary and Data. It must not touch provenance: the
// gateway stamps that, so a tool cannot vouch for its own evidence.
type Shaper interface {
	Shape(raw map[string]any, ev *ops.Evidence) error
}

// ShaperFunc adapts a function to Shaper.
type ShaperFunc func(raw map[string]any, ev *ops.Evidence) error

func (f ShaperFunc) Shape(raw map[string]any, ev *ops.Evidence) error { return f(raw, ev) }

// Registry is the subset of the tool registry the gateway reads.
type Registry interface {
	Lookup(name string) (ops.ToolMetadata, bool)
}

// Errors callers can branch on.
var (
	// ErrUnknownTool means no registered name matched. It is a routing
	// failure, not a tool failure, and must not count against a tool's
	// success rate.
	ErrUnknownTool = errors.New("gateway: unknown tool")
	// ErrNotPermitted means admission rejected the call: the tool's side
	// effect exceeds what this session allows.
	ErrNotPermitted = errors.New("gateway: tool not permitted here")
	// ErrApprovalRequired means the call needs a human decision first.
	ErrApprovalRequired = errors.New("gateway: tool requires approval")
)

// Policy decides what may run. It is separate from metadata because the
// metadata says what a tool *is* while the policy says what this session may
// *do* — a read-only console and an on-call operator share a registry.
type Policy struct {
	// MaxSideEffect is the strongest effect permitted. Empty means read-only,
	// which is the safe default for a path that has not thought about it.
	MaxSideEffect ops.SideEffectLevel
	// AllowApprovalRequired permits tools flagged as needing approval, for a
	// caller that has its own approval flow.
	AllowApprovalRequired bool
}

// effectRank orders side effects so a policy can compare them.
func effectRank(l ops.SideEffectLevel) int {
	switch l {
	case ops.SideEffectRisky:
		return 2
	case ops.SideEffectMutating:
		return 1
	default:
		return 0
	}
}

// permits reports whether the policy admits a tool.
func (p Policy) permits(m ops.ToolMetadata) error {
	level := m.SideEffect
	if level == "" {
		// An MCP tool that declares nothing is not assumed safe; a built-in is.
		if m.Server != "" {
			level = ops.SideEffectMutating
		} else {
			level = ops.SideEffectReadOnly
		}
	}
	if effectRank(level) > effectRank(p.MaxSideEffect) {
		return fmt.Errorf("%w: %s 的副作用等级为 %s，本次仅允许 %s",
			ErrNotPermitted, m.Name, level, orReadOnly(p.MaxSideEffect))
	}
	if m.NeedsApproval && !p.AllowApprovalRequired {
		reason := m.ApprovalReason
		if reason == "" {
			reason = "该工具被标记为需要人工确认"
		}
		return fmt.Errorf("%w: %s（%s）", ErrApprovalRequired, m.Name, reason)
	}
	return nil
}

func orReadOnly(l ops.SideEffectLevel) ops.SideEffectLevel {
	if l == "" {
		return ops.SideEffectReadOnly
	}
	return l
}

// Config wires a gateway.
type Config struct {
	Registry Registry
	// Executors run tools, keyed by MCP server id; the empty key serves
	// built-in tools.
	Executors map[string]Executor
	// Shapers reduce results, keyed by our canonical tool name. A tool with
	// no shaper gets the default: its payload is passed through, bounded by
	// the metadata's byte ceiling.
	Shapers map[string]Shaper
	Policy  Policy
}

// Gateway executes tool calls under policy, with arguments injected,
// results shaped, and evidence minted.
//
// Safe for concurrent use. Calls and evidence are numbered from separate
// counters: sharing one would leave gaps in the evidence sequence (e2, e4, e6),
// and a reader following a citation chain would take a gap for a lost
// observation.
type Gateway struct {
	reg       Registry
	executors map[string]Executor
	shapers   map[string]Shaper
	policy    Policy

	callSeq atomic.Uint64
	evSeq   atomic.Uint64
}

// New builds a gateway. A nil registry yields one that rejects every call with
// ErrUnknownTool, which is the correct degraded behaviour: better to route
// nothing than to route blindly.
func New(cfg Config) *Gateway {
	return &Gateway{
		reg:       cfg.Registry,
		executors: cfg.Executors,
		shapers:   cfg.Shapers,
		policy:    cfg.Policy,
	}
}

// Result is one completed call: the audit record, and the evidence it produced.
//
// Evidence is nil when the call failed — there is no observation to cite. The
// ToolCall is present either way, because a success rate computed only over
// successes is not a success rate.
type Result struct {
	Call     ops.ToolCall
	Evidence *ops.Evidence
}

// Execute runs one tool call end to end.
//
// name is whatever the model wrote: a canonical name or any alias. args are
// the model's arguments, which may use argument aliases and must not contain
// injected parameters — anything it supplies for those is discarded, since the
// context is authoritative and a model-supplied cluster is exactly the
// confident mistake this design removes.
func (g *Gateway) Execute(ctx context.Context, ic *ops.IncidentContext, origin ops.Origin, name string, args map[string]any) (Result, error) {
	started := time.Now()
	call := ops.ToolCall{
		ID:        "c" + strconv.FormatUint(g.callSeq.Add(1), 10),
		Tool:      name,
		Origin:    origin,
		StartedAt: started,
	}

	m, ok := g.lookup(name)
	if !ok {
		call.Duration = time.Since(started)
		call.ErrKind = "unknown_tool"
		call.Err = fmt.Sprintf("%v: %s", ErrUnknownTool, name)
		return Result{Call: call}, fmt.Errorf("%w: %s", ErrUnknownTool, name)
	}
	// From here on the call is attributed to our canonical name, not to
	// whichever alias the model happened to use — otherwise the same tool
	// would appear under several names in every statistic.
	call.Tool = m.Name

	if err := g.policy.permits(m); err != nil {
		call.Duration = time.Since(started)
		call.ErrKind = "denied"
		call.Err = err.Error()
		return Result{Call: call}, err
	}

	exec, ok := g.executors[m.Server]
	if !ok {
		call.Duration = time.Since(started)
		call.ErrKind = "no_executor"
		call.Err = fmt.Sprintf("gateway: 没有为 server %q 配置执行器", m.Server)
		return Result{Call: call}, errors.New(call.Err)
	}

	final := PrepareArgs(m, ic, args)
	call.Args = final

	runCtx := ctx
	if m.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, m.Timeout)
		defer cancel()
	}

	raw, err := exec.Execute(runCtx, m.Remote(), final)
	call.Duration = time.Since(started)
	if err != nil {
		call.Err = err.Error()
		call.ErrKind = classifyExecError(err)
		return Result{Call: call}, err
	}
	if msg := payloadError(raw); msg != "" {
		// A tool reporting its failure inside an otherwise-successful payload
		// is still a failure. Missing this is how a success rate reads 100%.
		call.Err = msg
		call.ErrKind = "tool_error"
		return Result{Call: call}, errors.New(msg)
	}

	call.OK = true
	call.ResultBytes = payloadSize(raw)

	ev := &ops.Evidence{
		ID:         g.nextEvidenceID(),
		Source:     ops.Source{Backend: m.Backend, Tool: m.Name, Server: m.Server},
		Args:       final,
		Window:     windowOf(ic),
		ObservedAt: time.Now(),
		Origin:     origin,
		Kind:       ops.KindText,
	}
	if err := g.shape(m, raw, ev); err != nil {
		// Shaping failed, but the call succeeded: report the raw payload
		// rather than discard an observation we paid for.
		ev.Summary = fmt.Sprintf("结果整形失败（%v），以下为原始返回", err)
		ev.Data = rawJSON(raw)
	}
	if m.MaxResultBytes > 0 && len(ev.Data) > m.MaxResultBytes {
		ev.Data = ev.Data[:m.MaxResultBytes]
		ev.Truncated = true
	}
	call.Truncated = ev.Truncated
	call.EvidenceIDs = []string{ev.ID}
	return Result{Call: call, Evidence: ev}, nil
}

func (g *Gateway) lookup(name string) (ops.ToolMetadata, bool) {
	if g.reg == nil {
		return ops.ToolMetadata{}, false
	}
	return g.reg.Lookup(name)
}

func (g *Gateway) shape(m ops.ToolMetadata, raw map[string]any, ev *ops.Evidence) error {
	if s, ok := g.shapers[m.Name]; ok && s != nil {
		return s.Shape(raw, ev)
	}
	// Default shaping keeps the payload but records the tool's declared
	// evidence kind, so an unshaped tool is still classifiable.
	if m.Produces != "" {
		ev.Kind = m.Produces
	}
	ev.Summary = defaultSummary(raw)
	ev.Data = rawJSON(raw)
	return nil
}

// nextEvidenceID mints a short, gapless, ordered identifier.
//
// Short because these appear in prompts and reports, where every token counts.
// Gapless and ordered because a reader following a causal chain reads e3 as
// having been observed before e7 — and reads a missing e5 as an observation
// that went astray.
func (g *Gateway) nextEvidenceID() string {
	return "e" + strconv.FormatUint(g.evSeq.Add(1), 10)
}

// PrepareArgs builds the final argument map: aliases canonicalized, injected
// parameters filled from context, model-supplied values for those discarded.
//
// Exported because selection needs the same view to render a schema with the
// injected parameters removed — the model must not see a parameter it is not
// allowed to set, or it will set it.
func PrepareArgs(m ops.ToolMetadata, ic *ops.IncidentContext, args map[string]any) map[string]any {
	out := make(map[string]any, len(args)+4)

	for k, v := range args {
		canonical := m.CanonicalArg(k)
		if m.Injects(canonical) {
			continue // context is authoritative
		}
		out[canonical] = v
	}

	// Handle fields for this tool's backend: cluster, namespace, instance…
	if h, ok := ic.HandleFor(m.Backend); ok {
		for k, v := range h.Ref {
			if m.Injects(k) {
				out[k] = v
			}
		}
	}

	// The incident window, so no tool invents its own "last 15 minutes".
	if m.WindowParams[0] != "" || m.WindowParams[1] != "" {
		w := windowOf(ic)
		if m.WindowParams[0] != "" {
			out[m.WindowParams[0]] = w.Since.Format(time.RFC3339)
		}
		if m.WindowParams[1] != "" {
			out[m.WindowParams[1]] = w.Until.Format(time.RFC3339)
		}
	}
	return out
}

func windowOf(ic *ops.IncidentContext) ops.TimeWindow {
	if ic == nil || ic.Window.Until.IsZero() {
		return ops.DefaultWindow()
	}
	return ic.Window
}

// payloadError extracts the error a tool reports inside its own result. ADK
// signals a failed tool this way: the call completes at the protocol level and
// the payload carries an "error" key.
func payloadError(raw map[string]any) string {
	if raw == nil {
		return ""
	}
	v, ok := raw["error"]
	if !ok || v == nil {
		return ""
	}
	switch e := v.(type) {
	case string:
		return e
	case error:
		return e.Error()
	default:
		return fmt.Sprintf("%v", e)
	}
}

func classifyExecError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "upstream"
	}
}

func payloadSize(raw map[string]any) int {
	if len(raw) == 0 {
		return 0
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return 0
	}
	return len(b)
}

func rawJSON(raw map[string]any) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	return b
}

// defaultSummary picks a human-readable line from a payload that has no
// shaper. It looks for the conventional keys rather than dumping the map,
// because the summary is what reaches the prompt.
func defaultSummary(raw map[string]any) string {
	for _, k := range []string{"summary", "title", "text", "content", "result"} {
		if v, ok := raw[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return fmt.Sprintf("返回 %d 个字段", len(raw))
}

// Snapshot adapts a registry snapshot to the gateway's Registry interface.
func Snapshot(r *toolreg.Registry) Registry { return registrySnapshot{r} }

type registrySnapshot struct{ r *toolreg.Registry }

func (s registrySnapshot) Lookup(name string) (ops.ToolMetadata, bool) { return s.r.Lookup(name) }
