// Package gateway governs a tool call: admission, arguments, naming, shaping,
// provenance.
//
// It is not yet the only path a call can take, and that gap is worth stating
// plainly rather than aspiring past. Built-in tools go through it. MCP tools
// and anything without metadata still reach ADK directly, so for those there
// is no argument injection, no shaping, and no evidence — see WrapTools for
// why an MCP toolset needs a different wrapper, and reportUnregistered in
// internal/engine for how the remainder is surfaced instead of hidden.
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
	"sort"
	"strconv"
	"strings"
	"sync"
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

// effectRank orders the three known levels.
//
// It is only ever called on a value already checked by Valid, so it has no
// case for anything else. An earlier version ranked unknown values above
// everything, which looked safe and was not: an unset policy is also an
// unknown value, so it ranked highest too and therefore admitted everything.
// The empty string means two different things — a tool that declared nothing,
// and a policy nobody configured — and conflating them in one ranking is what
// produced that hole. Both are now resolved to a real level before comparison.
func effectRank(l ops.SideEffectLevel) int {
	switch l {
	case ops.SideEffectRisky:
		return 2
	case ops.SideEffectMutating:
		return 1
	default: // read_only
		return 0
	}
}

// permits reports whether the policy admits a tool.
// permits reports whether the policy admits a tool, and returns the level it
// resolved — callers need it to decide whether a timeout may abandon the call.
func (p Policy) permits(m ops.ToolMetadata) (ops.SideEffectLevel, error) {
	// Resolve the tool's level first. Unset is not unknown: a built-in that
	// declares nothing is read-only, while an MCP tool that declares nothing
	// is assumed to mutate, because a third-party server's silence is not a
	// safety guarantee.
	level := m.SideEffect
	if level == "" {
		if m.Server != "" {
			level = ops.SideEffectMutating
		} else {
			level = ops.SideEffectReadOnly
		}
	}
	if !level.Valid() {
		// Naming the bad value matters: the fix is a one-character edit in a
		// YAML file, and an operator cannot make it from "not permitted".
		return level, fmt.Errorf("%w: %s 声明了无法识别的副作用等级 %q（应为 %s / %s / %s），已按最高风险拒绝",
			ErrNotPermitted, m.Name, level,
			ops.SideEffectReadOnly, ops.SideEffectMutating, ops.SideEffectRisky)
	}
	// Resolve the policy's ceiling. Unset means read-only — the safe default
	// for a call path that has not thought about it — and a misspelt ceiling
	// is refused outright rather than guessed at.
	ceiling := p.MaxSideEffect
	if ceiling == "" {
		ceiling = ops.SideEffectReadOnly
	}
	if !ceiling.Valid() {
		return level, fmt.Errorf("%w: 本次策略的 MaxSideEffect %q 无法识别（应为 %s / %s / %s）",
			ErrNotPermitted, p.MaxSideEffect,
			ops.SideEffectReadOnly, ops.SideEffectMutating, ops.SideEffectRisky)
	}

	if effectRank(level) > effectRank(ceiling) {
		return level, fmt.Errorf("%w: %s 的副作用等级为 %s，本次仅允许 %s",
			ErrNotPermitted, m.Name, level, ceiling)
	}
	if m.NeedsApproval && !p.AllowApprovalRequired {
		reason := m.ApprovalReason
		if reason == "" {
			reason = "该工具被标记为需要人工确认"
		}
		return level, fmt.Errorf("%w: %s（%s）", ErrApprovalRequired, m.Name, reason)
	}
	return level, nil
}

// Sink receives every completed call and the evidence it produced.
//
// It exists because the gateway is the only place that knows what actually
// happened: the arguments after injection, the canonical tool name rather than
// whichever alias the model used, the cause bucket, and the evidence ID a
// conclusion will cite. Recording from ADK's tool callback instead captures
// the model's own arguments — a record of what was asked for, not of what was
// done, and one that cannot be checked against the evidence at all.
//
// Called synchronously on every path out of a call, so an implementation must
// be quick and must not fail: a recorder is observability, the call is the
// product.
type Sink interface {
	Record(ctx context.Context, meta CallMeta, res Result)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(ctx context.Context, meta CallMeta, res Result)

func (f SinkFunc) Record(ctx context.Context, meta CallMeta, res Result) { f(ctx, meta, res) }

// CallMeta identifies the conversation a call belongs to. The gateway does not
// read it; it passes it through so a sink can attribute the row.
type CallMeta struct {
	SessionID    string
	InvocationID string
	Agent        string
	CallID       string
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
	// Sink records completed calls. Optional; nil records nothing.
	Sink Sink
}

// Gateway executes tool calls under policy, with arguments injected,
// results shaped, and evidence minted.
//
// Safe for concurrent use. Calls and evidence are numbered from separate
// counters: sharing one would leave gaps in the evidence sequence (e2, e4, e6),
// and a reader following a citation chain would take a gap for a lost
// observation.
type Gateway struct {
	reg     Registry
	shapers map[string]Shaper
	policy  Policy
	sink    Sink

	// executors is guarded because it is filled after construction: the
	// gateway is built once per engine, while the executor for a set of tools
	// can only be made once those tools are assembled. Reads happen on every
	// call from many goroutines, so this is a value swap rather than a
	// per-call lock.
	execMu    sync.RWMutex
	executors map[string]Executor

	// discovered holds metadata synthesized for tools a server actually
	// exposed but nobody described. It is kept apart from the registry
	// because the two have different standing: the registry is what someone
	// decided, this is what was found. A declared entry therefore always
	// wins, and adopting a tool can never quietly redefine a configured one.
	//
	// It is filled at bind time from a real tool list, never from a name
	// alone. A registry that answered every lookup would also answer for a
	// name the model invented, and the gateway would then try to route a
	// tool that does not exist.
	discMu sync.RWMutex
	// discovered maps our canonical name to the metadata.
	discovered map[string]ops.ToolMetadata
	// adopted maps "server/remote" to the canonical name already chosen for
	// it, so re-binding the same toolset — which happens every turn — keeps
	// the name stable instead of allocating a new one each time.
	adopted map[string]string

	callSeq atomic.Uint64
	evSeq   atomic.Uint64
}

// New builds a gateway. A nil registry yields one that rejects every call with
// ErrUnknownTool, which is the correct degraded behaviour: better to route
// nothing than to route blindly.
func New(cfg Config) *Gateway {
	execs := make(map[string]Executor, len(cfg.Executors))
	for k, v := range cfg.Executors {
		execs[k] = v
	}
	return &Gateway{
		reg:       cfg.Registry,
		executors: execs,
		shapers:   cfg.Shapers,
		policy:    cfg.Policy,
		sink:      cfg.Sink,
	}
}

// SetExecutor registers the executor for one server, replacing any previous
// one. The empty key serves built-in tools.
//
// It exists because of an ordering constraint: the gateway is built once per
// engine, but an executor over a set of ADK tools can only be made after those
// tools are assembled — which happens per agent build.
func (g *Gateway) SetExecutor(server string, e Executor) {
	g.execMu.Lock()
	defer g.execMu.Unlock()
	if g.executors == nil {
		g.executors = map[string]Executor{}
	}
	g.executors[server] = e
}

func (g *Gateway) executor(server string) (Executor, bool) {
	g.execMu.RLock()
	defer g.execMu.RUnlock()
	e, ok := g.executors[server]
	return e, ok
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
	return g.ExecuteAs(ctx, CallMeta{}, ic, origin, name, args)
}

// ExecuteAs is Execute with the conversation identifiers a sink needs to
// attribute the row.
func (g *Gateway) ExecuteAs(ctx context.Context, meta CallMeta, ic *ops.IncidentContext, origin ops.Origin, name string, args map[string]any) (res Result, err error) {
	started := time.Now()
	// Recorded on every path out, including the early rejections: a call that
	// was denied or unroutable is part of what happened, and leaving those out
	// of the record is how a success rate ends up describing only the calls
	// that got as far as running.
	defer func() {
		if g.sink != nil {
			g.sink.Record(ctx, meta, res)
		}
	}()
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

	level, err := g.policy.permits(m)
	if err != nil {
		call.Duration = time.Since(started)
		call.ErrKind = "denied"
		call.Err = err.Error()
		return Result{Call: call}, err
	}

	exec, ok := g.executor(m.Server)
	if !ok {
		call.Duration = time.Since(started)
		call.ErrKind = "no_executor"
		call.Err = fmt.Sprintf("gateway: 没有为 server %q 配置执行器", m.Server)
		return Result{Call: call}, errors.New(call.Err)
	}

	final := PrepareArgs(m, ic, args)
	call.Args = final

	// Only a read-only call may be abandoned on timeout. See runWithTimeout.
	raw, err := runWithTimeout(ctx, m.Timeout, level == ops.SideEffectReadOnly,
		func(runCtx context.Context) (map[string]any, error) {
			return exec.Execute(runCtx, m.Remote(), final)
		})
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
	if bounded, cut := boundPayload(ev.Data, m.MaxResultBytes); cut {
		// A nil result is deliberate, not a failure: below a few bytes there
		// is no payload worth carrying, and Truncated says so.
		ev.Data = bounded
		ev.Truncated = true
	}
	if bounded, cut := boundSummary(ev.Summary, m.MaxResultBytes); cut {
		// The summary is what actually reaches the prompt, so a ceiling that
		// only bounded Data would let a tool returning one enormous content
		// field walk straight past the context budget.
		ev.Summary = bounded
		ev.Truncated = true
	}
	call.Truncated = ev.Truncated
	call.EvidenceIDs = []string{ev.ID}
	return Result{Call: call, Evidence: ev}, nil
}

func (g *Gateway) lookup(name string) (ops.ToolMetadata, bool) {
	if g.reg != nil {
		if m, ok := g.reg.Lookup(name); ok {
			return m, true
		}
	}
	g.discMu.RLock()
	defer g.discMu.RUnlock()
	m, ok := g.discovered[name]
	return m, ok
}

// adopt gives a discovered-but-undeclared tool a canonical name and records
// its metadata, returning the metadata as the gateway will see it.
//
// Naming rule: keep the name the server gave it, and only prefix on an actual
// collision. Renaming pre-emptively would cost what the metadata doc warns
// about — the name is a hint the model reads, and "k8s__get_pods" selects
// worse than "get_pods" — but a collision here is not hypothetical. Two MCP
// servers both exposing get_pods, neither described, is the ordinary case, and
// leaving them to share one name would route half the calls to the wrong
// cluster while looking like it worked.
//
// Calling it again for the same (server, remote) returns the same name: a
// toolset is re-bound every turn, and a name that drifted between turns would
// invalidate the model's own memory of what it just called.
func (g *Gateway) adopt(m ops.ToolMetadata) (ops.ToolMetadata, error) {
	key := m.Server + "/" + m.Remote()

	g.discMu.Lock()
	defer g.discMu.Unlock()
	if name, ok := g.adopted[key]; ok {
		return g.discovered[name], nil
	}

	name := m.Remote()
	if g.nameTakenLocked(name) {
		if m.Server == "" {
			// A built-in colliding with a declared entry is a programming
			// error, not a deployment one, and there is no server to prefix
			// with. Refusing beats inventing a name for it.
			return m, fmt.Errorf("gateway: 内置工具 %q 与已声明的条目重名", name)
		}
		name = m.Server + "__" + m.Remote()
		if g.nameTakenLocked(name) {
			return m, fmt.Errorf("gateway: %s 与 %s 都已被占用，%s/%s 无法接管",
				m.Remote(), name, m.Server, m.Remote())
		}
	}
	m.Name = name

	if g.discovered == nil {
		g.discovered = map[string]ops.ToolMetadata{}
		g.adopted = map[string]string{}
	}
	g.discovered[name] = m
	g.adopted[key] = name
	return m, nil
}

// nameTakenLocked reports whether a canonical name is already claimed, by a
// declared entry or by an earlier adoption. Callers hold discMu.
func (g *Gateway) nameTakenLocked(name string) bool {
	if g.reg != nil {
		if _, ok := g.reg.Lookup(name); ok {
			return true
		}
	}
	_, ok := g.discovered[name]
	return ok
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

// runWithTimeout bounds a read-only call at the tool's declared timeout.
//
// Abandoning a call is only safe when it cannot change anything. ADK's
// ToolContext carries no deadline (only InvocationContext has WithContext), so
// a tool that ignores its context keeps running after we stop waiting — and
// for a mutating tool that produces the worst possible outcome: the caller
// reports a failure, the write lands a moment later, and a retry does it
// twice. "Restart failed" followed by two restarts is not a timeout, it is a
// duplicated side effect.
//
// So only read-only tools are abandoned. A mutating tool is awaited to
// completion, on the reasoning that an honest "this took 40 seconds" beats a
// false "this failed" about a change that actually happened. Bounding those
// properly needs cancellation the tool respects — which is a property of the
// tool, not something a caller can add.
func runWithTimeout(ctx context.Context, timeout time.Duration, abandonable bool, fn func(context.Context) (map[string]any, error)) (map[string]any, error) {
	if timeout <= 0 {
		return fn(ctx)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	if !abandonable {
		// Awaited inline: the deadline still reaches a tool that honours its
		// context, but one that does not is allowed to finish.
		defer cancel()
		return fn(runCtx)
	}

	type outcome struct {
		result map[string]any
		err    error
	}
	// Buffered so the abandoned goroutine can finish and exit; cancel is
	// deferred to the goroutine rather than this function, or abandoning the
	// call would cancel a context the goroutine is still using.
	done := make(chan outcome, 1)
	go func() {
		defer cancel()
		result, err := fn(runCtx)
		done <- outcome{result, err}
	}()

	select {
	case o := <-done:
		return o.result, o.err
	case <-runCtx.Done():
		return nil, runCtx.Err()
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

// maxSummaryChars caps a summary even when a tool declares no ceiling.
//
// A summary is one line in a prompt and one line in a report. Nothing legible
// needs more than this, and a tool returning a whole document in its "content"
// field would otherwise spend the context budget by itself.
const maxSummaryChars = 600

// truncMarker tells a reader the text is an excerpt. Without it an excerpt
// reads as the whole thing, which is worse than a longer excerpt.
const truncMarker = "…（已截断）"

// boundPayload keeps a JSON payload under a byte ceiling while leaving it
// valid JSON.
//
// Slicing the bytes is what the obvious implementation does and it produces
// invalid JSON — worse, an unmarshalable json.RawMessage makes the whole
// Evidence, and therefore the whole DiagnosisResult, fail to serialize. So the
// payload is re-encoded with its string values shortened instead, and if even
// that does not fit it is replaced by a note rather than by a broken fragment.
func boundPayload(data json.RawMessage, maxBytes int) (json.RawMessage, bool) {
	if maxBytes <= 0 || len(data) <= maxBytes {
		return data, false
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		// Not JSON we can walk; a note beats a truncated fragment.
		return droppedNote(maxBytes), true
	}
	// Shrink the string leaves until the encoding fits. Halving converges in
	// a few passes and keeps the structure — which is the part a reader needs
	// to see — intact.
	limit := maxBytes
	for i := 0; i < 12; i++ {
		limit /= 2
		if limit < 16 {
			break
		}
		shrunk := shrinkStrings(v, limit)
		out, err := json.Marshal(shrunk)
		if err != nil {
			break
		}
		if len(out) <= maxBytes {
			return out, true
		}
	}
	return droppedNote(maxBytes), true
}

// droppedNote is the last resort when nothing legible fits.
//
// It obeys the ceiling too. A fallback that overruns the very limit it is
// enforcing is not a fallback — and below a handful of bytes the honest answer
// is no payload at all: Evidence.Data is optional, Truncated already records
// that something was dropped, and Summary still carries the observation.
func droppedNote(maxBytes int) json.RawMessage {
	for _, note := range []string{
		`{"truncated":"结果超出上限，已省略"}`,
		`{"truncated":"已省略"}`,
		`{"truncated":true}`,
	} {
		if len(note) <= maxBytes {
			return json.RawMessage(note)
		}
	}
	return nil
}

// shrinkStrings walks a decoded payload and shortens every string leaf.
func shrinkStrings(v any, limit int) any {
	switch t := v.(type) {
	case string:
		return truncRunes(t, limit)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = shrinkStrings(val, limit)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = shrinkStrings(val, limit)
		}
		return out
	default:
		return v
	}
}

// boundSummary caps the line that reaches the prompt.
func boundSummary(summary string, maxBytes int) (string, bool) {
	limit := maxSummaryChars
	if maxBytes > 0 && maxBytes < limit {
		limit = maxBytes
	}
	if len([]rune(summary)) <= limit {
		return summary, false
	}
	return truncRunes(summary, limit), true
}

// truncRunes cuts on a rune boundary, so a multi-byte character is never split
// into invalid UTF-8, and marks the result as an excerpt.
func truncRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	if limit <= 0 {
		return truncMarker
	}
	return string(r[:limit]) + truncMarker
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

// defaultSummary picks a readable line from a payload that has no shaper.
//
// The conventional text keys come first, then a compact rendering of the
// scalar fields. "返回 2 个字段" was the earlier fallback and it is worse than
// nothing: the summary is the line that reaches the prompt and the report, so
// a model reading it learns only that something happened. Naming the values it
// found at least says what.
func defaultSummary(raw map[string]any) string {
	for _, k := range []string{"summary", "title", "text", "content", "result", "message"} {
		if v, ok := raw[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	if len(raw) == 0 {
		return "无返回内容"
	}

	// Scalars in sorted order, so the same payload always reads the same way.
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		switch v := raw[k].(type) {
		case string, bool, float64, int, int64, json.Number:
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
		if len(parts) >= 6 {
			break
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("返回 %d 个结构化字段（详见 data）", len(raw))
	}
	return strings.Join(parts, " ")
}

// Snapshot adapts a registry snapshot to the interfaces this package reads.
func Snapshot(r *toolreg.Registry) KeyedRegistry { return registrySnapshot{r} }

type registrySnapshot struct{ r *toolreg.Registry }

func (s registrySnapshot) Lookup(name string) (ops.ToolMetadata, bool) { return s.r.Lookup(name) }

func (s registrySnapshot) ByKey(key string) (ops.ToolMetadata, bool) { return s.r.ByKey(key) }
