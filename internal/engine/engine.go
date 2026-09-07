// Package engine assembles jelly-agent's runtime — model, agent, runner,
// session store, and memory layers — from a loaded config. It is the single
// source of truth shared by the CLI (cmd/cli) and the web server (cmd/server),
// so both drive the same agent the same way.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	adksession "google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/gateway"
	"github.com/jelly-agent/jelly-agent/internal/history"
	jellymcp "github.com/jelly-agent/jelly-agent/internal/mcp"
	"github.com/jelly-agent/jelly-agent/internal/memory"
	jellymetrics "github.com/jelly-agent/jelly-agent/internal/metrics"
	jellymodel "github.com/jelly-agent/jelly-agent/internal/model"
	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/record"
	"github.com/jelly-agent/jelly-agent/internal/sandbox"
	"github.com/jelly-agent/jelly-agent/internal/selector"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	"github.com/jelly-agent/jelly-agent/internal/skill"
	jellytelemetry "github.com/jelly-agent/jelly-agent/internal/telemetry"
	jellytool "github.com/jelly-agent/jelly-agent/internal/tool"
	"github.com/jelly-agent/jelly-agent/internal/toolreg"

	"github.com/jelly-agent/jelly-agent/internal/logging"
)

// Wire a default audit sink so every sandboxed script run leaves a log line for
// review (PLAN §8 risk 6). The app may override sandbox.Audit to redirect it.
func init() {
	sandbox.Audit = func(ev sandbox.AuditEvent) {
		status := "exit=" + fmt.Sprint(ev.ExitCode)
		if ev.TimedOut {
			status = "timeout"
		} else if ev.Err != "" {
			status = "start-error: " + ev.Err
		}
		slog.Info("沙箱执行",
			"backend", ev.Backend, "file", ev.File, "args", ev.Args,
			"status", status, "duration_ms", ev.Duration.Milliseconds())
	}
}

const (
	// AppName and UserID scope sessions and memory. The CLI is single-user, so
	// UserID is a constant; the web server reuses the same identity.
	AppName = "jelly-agent"
	UserID  = "local-user"

	// RootInstruction is the static base instruction. L1 core memory is
	// prepended to it each turn via the InstructionProvider (PLAN §10.1).
	RootInstruction = "你是 jelly-agent，一个用 Go + ADK-Go 构建的助手。" +
		"需要实时或外部信息时调用 web_search 工具，再用中文简洁作答。" +
		"当用户表达偏好、身份或重要约定，值得跨会话记住时，调用 remember 工具；" +
		"信息过时或用户要求忘记时，调用 forget。不要重复记录上文「长期记忆」中已有的内容。" +
		"需要回忆过去对话时，调用 load_memory 检索历史。"
)

// Engine builds runtime components from a config. The search index and session
// store own their own handles; the engine additionally owns the lifetime of any
// stdio MCP subprocesses, which is why it carries a cancelable context and a
// Close. MCP toolsets are built once and cached so their sessions persist across
// the many per-request agent builds the web server performs.
type Engine struct {
	cfg *config.Config
	reg *jellymodel.Registry

	mcpCtx     context.Context
	mcpCancel  context.CancelFunc
	mcpOnce    sync.Once
	mcpSets    map[string]adktool.Toolset // enabled MCP toolsets, keyed by server name
	extraTools []adktool.Tool

	// Tool-call telemetry. Built once and shared by every agent this engine
	// builds, so the in-flight table pairs a call's start and finish even when
	// the web server rebuilds the agent between them.
	metricsOnce sync.Once
	metrics     *jellymetrics.Tracker

	// sessionDBPath overrides the shared store's location. Empty means the
	// default path; only tests and embedders set it.
	sessionDBPath string

	// Tool registry and gateway. Built once per engine: the registry snapshot
	// is immutable and the gateway is safe for concurrent use, so the many
	// per-request agent builds the web server performs all share them.
	toolsOnce sync.Once
	toolStore *toolreg.Store
	gw        *gateway.Gateway
	// undeclared names the tools no metadata covers, per server, so an
	// operator can see what is running on synthesized defaults.
	//
	// Keyed by server and logged once per server rather than once overall: a
	// toolset re-binds every turn, and a single sync.Once would have logged
	// whichever server happened to bind first and stayed quiet about the rest.
	undeclaredMu sync.Mutex
	undeclared   map[string][]string

	// admit keeps each session's tool set stable, to protect the prompt cache.
	admitOnce sync.Once
	admit     *admissions

	// budget tracks how much room each round has left for tool results.
	budgetOnce sync.Once
	budget     *resultBudget

	// health remembers which MCP servers are not answering.
	healthOnce sync.Once
	health     *toolsetHealth

	// listing collapses concurrent tool-list fetches per server.
	listingOnce sync.Once
	listing     *listing

	// recordStore durably keeps tool deliveries so a shortened result can be
	// read back.
	recordsOnce sync.Once
	recordStore *record.Store
	recordsErr  error
}

// New wraps a loaded config in an engine.
func New(cfg *config.Config) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{cfg: cfg, reg: jellymodel.NewRegistry(cfg), mcpCtx: ctx, mcpCancel: cancel}
}

// Config returns the underlying config.
func (e *Engine) Config() *config.Config             { return e.cfg }
func (e *Engine) SetExtraTools(tools []adktool.Tool) { e.extraTools = tools }

// Close releases engine-owned resources. It cancels the MCP context, which
// terminates any stdio MCP subprocesses this engine launched. Safe to call once
// the engine is no longer serving requests (e.g. after a config hot-reload).
func (e *Engine) Close() {
	if e.mcpCancel != nil {
		e.mcpCancel()
	}
	if err := e.metrics.Close(); err != nil {
		slog.Warn("关闭指标存储失败", logging.Err(err))
	}
}

// buildToolsets builds the enabled MCP toolsets once and caches them by server
// name, so repeated agent builds reuse the same (lazily-connected) MCP sessions.
// A server whose transport config is invalid is skipped rather than failing the
// whole set.
func (e *Engine) buildToolsets() {
	e.mcpOnce.Do(func() {
		e.mcpSets = map[string]adktool.Toolset{}
		for _, srv := range e.cfg.MCP {
			if !srv.Enabled {
				continue
			}
			ts, err := jellymcp.Toolset(e.mcpCtx, srv)
			if err != nil {
				continue // bad config (e.g. missing command/url); skip this server
			}
			e.mcpSets[srv.Name] = ts
		}
	})
}

// NamedToolset pairs an MCP toolset with the server it was configured as.
//
// The pairing has to be carried explicitly because ADK's mcptoolset reports a
// constant name — "mcp_tool_set" for every instance — so the only identifier
// the Toolset interface offers cannot tell two servers apart. Executor
// registration, routing keys and duplicate-name resolution all need the real
// one.
type NamedToolset struct {
	Name string
	Set  adktool.Toolset
}

// Toolsets returns every enabled MCP toolset (used by the web chat / default
// agent, which loads all of them).
func (e *Engine) Toolsets() []NamedToolset {
	e.buildToolsets()
	out := make([]NamedToolset, 0, len(e.mcpSets))
	for _, srv := range e.cfg.MCP { // stable order = config order
		if ts, ok := e.mcpSets[srv.Name]; ok {
			out = append(out, NamedToolset{Name: srv.Name, Set: ts})
		}
	}
	return out
}

// ToolsetsFor returns only the named enabled MCP toolsets, in the given order —
// the basis for a bot loading a selected subset of MCP servers.
func (e *Engine) ToolsetsFor(names []string) []NamedToolset {
	e.buildToolsets()
	out := make([]NamedToolset, 0, len(names))
	for _, n := range names {
		if ts, ok := e.mcpSets[n]; ok {
			out = append(out, NamedToolset{Name: n, Set: ts})
		}
	}
	return out
}

// SearchEnabled reports whether L2 session search is turned on in config.
func (e *Engine) SearchEnabled() bool { return e.cfg.Memory.Search.Enabled }

// Core builds the L1 core-memory store from config (or its defaults).
func (e *Engine) Core() (*memory.Core, error) {
	mc := e.cfg.Memory.Core
	return memory.NewCore(mc.Dir, mc.MemoryBudgetTokens, mc.UserBudgetTokens, mc.EnvBudgetTokens)
}

// Search builds the L2 FTS5 search service over the shared state.db when
// memory.search is enabled, returning nil otherwise. The caller owns Close.
func (e *Engine) Search() (*memory.Search, error) {
	if !e.cfg.Memory.Search.Enabled {
		return nil, nil
	}
	dbPath, err := jellysession.DefaultDBPath()
	if err != nil {
		return nil, err
	}
	s, err := memory.NewSearch(dbPath, e.cfg.Memory.Search.TopK)
	if err != nil {
		return nil, fmt.Errorf("init memory search: %w", err)
	}
	return s, nil
}

// toolRegistry builds the registry and gateway on first use.
//
// Metadata comes from two sources in order: the built-in defaults, then any
// YAML overlay. Order is significance — a later entry that clashes with an
// earlier one loses, and Build reports every such loss rather than dropping it
// silently.
//
// A metadata problem is logged and then tolerated. An unparsable overlay leaves
// the built-in defaults in place, because refusing to start over a malformed
// description would be a worse outcome than running with fewer wrapped tools.
func (e *Engine) toolRegistry() (*toolreg.Store, *gateway.Gateway) {
	e.toolsOnce.Do(func() {
		e.toolStore = toolreg.NewStore()

		sources := []toolreg.Source{jellytool.BuiltinMetadata()}
		// Always consulted, because the directory now has a default: the
		// declarations are how an MCP tool says what it produces, and a layer
		// reachable only by setting a path nobody knows about is a layer
		// nobody uses. A missing directory is not an error — it means nothing
		// has been declared yet.
		if dir := e.ToolMetadataDir(); dir != "" {
			sources = append(sources, toolreg.NewFileSource(dir))
		}
		e.toolStore.Swap(buildRegistry(sources))

		if e.contextUnguarded() {
			slog.Warn("上下文无任何上限保护：history.max_tokens 为 0 关闭了压缩，而 tools.max_result_bytes 为 0 不限制单次返回。"+
				"一个足够大的工具返回会直接让请求失败，且报错不会提到工具返回。二者至少开启一个。",
				"history_max_tokens", 0, "tools_max_result_bytes", 0)
		}

		// The ceiling is derived from what is actually configured, not set to
		// the widest level and left there.
		//
		// A fixed widest ceiling was the first version and it was wrong in a
		// specific way: it made the policy unable to deny anything, so the
		// whole mechanism read as decoration. Deriving it means a deployment
		// that never enables scripts gets a policy that would refuse one.
		//
		// AllowApprovalRequired stays on, and that is the remaining gap: no
		// tool sets NeedsApproval yet, so nothing is waved through today, but
		// the flag would wave it through the moment one did. Closing it needs
		// somewhere for a human to answer — ADK's RequestConfirmation into a
		// DingTalk card — which is its own change, not a smaller constant.
		e.gw = gateway.New(gateway.Config{
			Registry: gateway.Live(e.toolStore),
			Policy: gateway.Policy{
				MaxSideEffect:         e.sideEffectCeiling(),
				AllowApprovalRequired: true,
			},
			// The gateway records its own calls, because it is the only place
			// that knows the arguments after injection, the canonical tool
			// name, and the evidence ID a conclusion will cite. Recording from
			// the ADK callback captured the model's arguments instead — a
			// record of what was asked for rather than of what was done, and
			// one Seal's citations could not be checked against.
			Sink: gateway.SinkFunc(e.recordGatewayCall),
			// Durable, and distinct from the sink above: this one decides
			// whether a shortened result can be read back later, so its
			// failure changes what we may claim rather than merely losing a
			// row.
			Results: gateway.KeeperFunc(e.keepToolResult),
			// Decides per round whether a result still fits the prompt it is
			// joining. Only ever withholds a payload the store has already
			// committed, so nothing it declines is lost.
			Budget: e.resultBudget(),
		})
	})
	return e.toolStore, e.gw
}

// undeclaredFallback governs tools nobody has described.
//
// Govern is on, which is what makes "the gateway is the only path a tool call
// takes" true rather than aspirational: without it every MCP tool bypasses
// admission, telemetry and evidence, since an MCP server's tools are by
// definition not in the built-in metadata and are unlikely to have a YAML
// overlay before someone has needed one.
//
// Turning it on does not lock anything out that worked before. An undeclared
// remote tool resolves to mutating, and the ceiling is mutating unless scripts
// are enabled — so the ordinary deployment keeps running exactly the tools it
// ran yesterday, now with a record of each call. A deployment that deliberately
// sets a read-only ceiling is the one that will see MCP calls refused, which is
// the point of setting it.
//
// The result bound comes from config and is unbounded by default; see
// config.Tools.MaxResultBytes for why. The timeout stays fixed: a hanging
// server should not hang a turn, and unlike a byte ceiling a deadline cannot
// damage a result that does arrive.
// contextUnguarded reports that nothing bounds what reaches the context
// window.
//
// Two mechanisms can bound it and either is enough: history compaction, which
// shortens results once the prompt exceeds its budget, and the gateway's
// per-result byte ceiling. The ceiling is off by default on purpose (see
// config.Tools.MaxResultBytes), which is safe precisely because compaction is
// on by default. Turning compaction off as well leaves neither, and the
// symptom of that is a single enormous tool result failing the request with a
// provider error that names nothing about tool results.
// ContextUnguarded exposes the check for the console, so the settings form can
// say that neither bound is in force.
func (e *Engine) ContextUnguarded() bool { return e.contextUnguarded() }

func (e *Engine) contextUnguarded() bool {
	compactionOff := e.cfg.History.MaxTokens != nil && *e.cfg.History.MaxTokens <= 0
	return compactionOff && e.cfg.Tools.MaxResultBytes <= 0
}

func (e *Engine) undeclaredFallback() gateway.Fallback {
	return gateway.Fallback{
		Govern:         true,
		Timeout:        30 * time.Second,
		MaxResultBytes: e.cfg.Tools.MaxResultBytes,
	}
}

// SystemPrompt is the instruction the given agent sends on every turn, for a
// caller that wants to inspect what is being injected rather than run it.
//
// It exists so the console can show the prompt without a second assembly of
// it. Rebuilding it in the handler would mean two functions that have to agree
// about what the model sees, and the one that answers "what do we inject?"
// would be the one nobody notices drifting.
func (e *Engine) SystemPrompt(provider string) (parts []PromptPart, err error) {
	core, err := e.Core()
	if err != nil {
		return nil, err
	}
	mem, user := core.Snapshot()
	allow := e.cfg.Skills.AllowScripts

	parts = append(parts, PromptPart{Name: "指令", Text: RootInstruction})
	// Listed in the order it is injected, so the page reads the way the model
	// receives it. The environment block is first because it is what the rest
	// is reasoned against — and because an operator looking at this page is
	// usually asking "does it know X about our setup", which should be the
	// first thing they see.
	if env := core.Environment(); env != "" {
		parts = append(parts, PromptPart{Name: memory.EnvironmentFile, Text: env})
	}
	if mem != "" {
		parts = append(parts, PromptPart{Name: "MEMORY.md", Text: mem})
	}
	if user != "" {
		parts = append(parts, PromptPart{Name: "USER.md", Text: user})
	}
	if skills, err := e.Skills(); err == nil {
		if cat, cerr := skills.Catalog(); cerr == nil && cat != "" {
			parts = append(parts, PromptPart{Name: "技能目录", Text: cat})
		}
	}
	full := e.systemInstruction(core, RootInstruction, allow)
	parts = append(parts, PromptPart{Name: "完整拼装", Text: full, Assembled: true})
	return parts, nil
}

// PromptPart is one contribution to the system instruction.
type PromptPart struct {
	Name string `json:"name"`
	Text string `json:"text"`
	// Assembled marks the fully rendered result rather than an ingredient, so
	// a reader does not add its tokens to the ingredients' and double count.
	Assembled bool `json:"assembled,omitempty"`
}

// systemInstruction renders the per-turn system prompt.
//
// One implementation, used by the agent and by the console's prompt view. The
// alternative — the view rebuilding it — is how a page ends up confidently
// describing a prompt the model never received.
func (e *Engine) systemInstruction(core *memory.Core, instruction string, allowScripts bool) string {
	base := core.Render(instruction)
	if skills, err := e.Skills(); err == nil {
		if cat, err := skills.Catalog(); err == nil && cat != "" {
			base += "\n\n" + cat
			if allowScripts {
				base += "目录型技能可能附带脚本；需要时用 run_script 运行（凭据已由系统注入环境变量，按 use_skill 给出的 var_keys 在脚本里引用，切勿向用户索要密钥）。\n"
			}
		}
	}
	return base
}

// admissions returns the process-wide tool-admission record.
//
// It hangs off the engine rather than off a toolset because a toolset is built
// per request while the prompt cache it protects spans the conversation. One
// record per engine means a rebuild — a new request, a config reload of the
// agent tree — does not reset a session's set and cost it a cache miss.
func (e *Engine) admissions() *admissions {
	e.admitOnce.Do(func() { e.admit = newAdmissions() })
	return e.admit
}

// resultBudget returns the process-wide per-round result ledger.
//
// One per engine, for the same reason as the admission record: it is keyed by
// invocation, and an agent rebuilt per request must not lose the round it is
// in the middle of.
func (e *Engine) resultBudget() *resultBudget {
	e.budgetOnce.Do(func() { e.budget = newResultBudget() })
	return e.budget
}

// toolsetHealth returns the process-wide MCP liveness record.
//
// One per engine, like the admission record: it is keyed by server name and
// an agent rebuilt per request must not forget that a server is down.
func (e *Engine) toolsetHealth() *toolsetHealth {
	e.healthOnce.Do(func() { e.health = newToolsetHealth() })
	return e.health
}

// toolListing returns the process-wide in-flight tool-list collapser.
func (e *Engine) toolListing() *listing {
	e.listingOnce.Do(func() { e.listing = newListing() })
	return e.listing
}

// MCPHealth reports what is known about each MCP server's liveness.
//
// Exported for the console. A degraded turn is otherwise invisible from the
// browser: the server is skipped, the model answers without those tools, and
// "我查不到告警" reads as the agent being unable rather than as a host being
// unreachable.
func (e *Engine) MCPHealth() map[string]ServerHealth { return e.toolsetHealth().snapshot() }

// MaxTools exposes the resolved tool budget, for the console's prompt view.
func (e *Engine) MaxTools() int { return e.maxTools() }

// maxTools resolves the configured tool budget.
//
// Zero means "not configured" and takes the default; a negative number is the
// explicit way to turn selection off. The two are kept distinct because an
// unset int and a deliberate "send everything" are different intentions and a
// single zero cannot express both.
func (e *Engine) maxTools() int {
	switch n := e.cfg.Tools.MaxTools; {
	case n < 0:
		return 0 // selector.Config: no cap
	case n == 0:
		return defaultMaxTools
	default:
		return n
	}
}

// sideEffectCeiling is the strongest side effect this deployment permits.
//
// Derived rather than fixed: remember and forget write local memory on every
// deployment, so mutating is the floor; risky is admitted only when script
// execution is actually enabled, because run_script is the only tool that
// declares it. A deployment with scripts off therefore gets a policy that
// would refuse one — which is the difference between a permission check and a
// formality.
func (e *Engine) sideEffectCeiling() ops.SideEffectLevel {
	if e.cfg.Skills.AllowScripts {
		return ops.SideEffectRisky
	}
	return ops.SideEffectMutating
}

// keepToolResult commits what a tool delivered, before any shaping.
//
// Returning an error is the point: the gateway publishes a retrievable
// reference only when this succeeds. Nothing here is best-effort.
func (e *Engine) keepToolResult(ctx context.Context, meta gateway.CallMeta, tool string, delivered map[string]any) (string, error) {
	// The store's own readers are exempt. Their output already came out of the
	// store, so keeping a copy buys nothing and costs space — and it would
	// hand out handles to reads of reads, which is a chain nobody wants to
	// follow. Returning an empty label leaves the call marked not
	// retrievable, which is accurate: there is nothing new to retrieve.
	if tool == jellytool.ReadResultName || tool == jellytool.SearchResultName {
		return "", nil
	}
	store, err := e.records()
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(delivered)
	if err != nil {
		return "", fmt.Errorf("encode delivery: %w", err)
	}
	label, err := store.Put(ctx, record.Record{
		Scope: record.Scope{
			AppName: AppName, UserID: UserID, SessionID: meta.SessionID,
		},
		InvocationID: meta.InvocationID,
		CallID:       meta.CallID,
		Tool:         tool,
		At:           time.Now(),
		Payload:      payload,
		Upstream:     upstreamCut(delivered),
	})
	if err != nil {
		return "", err
	}
	// Counted only on success, so the stored/served ratio compares bytes that
	// are actually recoverable. Counting an attempt would make a failing store
	// look like one the model simply never reads from.
	jellytelemetry.RecordStoreBytes(ctx, jellytelemetry.RecordStored, tool, int64(len(payload)))
	return label, nil
}

// upstreamCut reads whether the tool had already shortened its own output.
//
// Three states, not two. Our own tools report it — fetch_url truncates to
// max_chars inside the tool and sets the flag — but a third-party MCP server
// has no such convention, and the flag also carries omitempty, so an absent
// key means "the tool did not say" rather than "the tool delivered
// everything". Recording that as No would turn a partial page into a promise
// of a complete one.
func upstreamCut(delivered map[string]any) record.Upstream {
	v, present := delivered["truncated"]
	if !present {
		return record.UpstreamUnknown
	}
	if b, ok := v.(bool); ok {
		if b {
			return record.UpstreamYes
		}
		return record.UpstreamNo
	}
	// Present but not a bool — a tool using the key for something else. Not a
	// claim we can read either way.
	return record.UpstreamUnknown
}

// records opens the delivery store on first use.
func (e *Engine) records() (*record.Store, error) {
	e.recordsOnce.Do(func() {
		path := e.sessionDBPath
		if path == "" {
			path, e.recordsErr = jellysession.DefaultDBPath()
			if e.recordsErr != nil {
				return
			}
		}
		e.recordStore, e.recordsErr = record.Open(path)
	})
	return e.recordStore, e.recordsErr
}

// Records exposes the delivery store, for the console's re-read endpoint.
func (e *Engine) Records() (*record.Store, error) { return e.records() }

// recordGatewayCall stores what the gateway actually did.
//
// A failed insert must not fail the call: the row is observability, the call is
// the product.
func (e *Engine) recordGatewayCall(_ context.Context, meta gateway.CallMeta, res gateway.Result) {
	row := jellymetrics.GatewayCall{
		SessionID: meta.SessionID, InvocationID: meta.InvocationID,
		Agent: meta.Agent, CallID: meta.CallID,

		Tool: res.Call.Tool, Args: res.Call.Args,
		StartedAt: res.Call.StartedAt, Duration: res.Call.Duration,
		OK: res.Call.OK, ErrKind: res.Call.ErrKind, Err: res.Call.Err,
		ResultBytes: res.Call.ResultBytes, Replayed: res.Call.Replayed,
		Retrievable: res.Call.Retrievable,
	}
	if res.Evidence != nil {
		row.EvidenceID = res.Evidence.ID
	}
	if rec := e.Metrics().Recorder(); rec != nil {
		if err := rec.RecordGatewayCall(row); err != nil {
			slog.Warn("工具调用记录写入失败", logging.Err(err), "tool", row.Tool)
		}
	}
	jellytelemetry.RecordToolCall(context.Background(), row.Tool, row.OK, row.ErrKind, row.Duration)
}

// incidentFor supplies the incident a tool call belongs to.
//
// There is no incident normalizer yet, so every call gets a context carrying
// only a default time window. That is already worth having: it is what stops
// each tool from inventing its own "last 15 minutes", which is the failure
// where an alert from three hours ago is diagnosed against a period when
// nothing was wrong. Targets and handles arrive with the normalizer.
func (e *Engine) incidentFor(agent.ToolContext) *ops.IncidentContext {
	return &ops.IncidentContext{
		Trigger: ops.TriggerUser,
		Window:  ops.DefaultWindow(),
	}
}

// reportUndeclared records and logs the tools no metadata covers.
//
// Reported rather than silent because these tools run on synthesized defaults:
// no use cases or anti-examples to select on, no declared side effect (so a
// remote one is assumed to mutate), and a generic timeout and result bound
// rather than sized ones. That is a working state, not a broken one, but it is
// the kind of gap that stays invisible until someone wonders why one tool's
// results are shaped and another's are not.
func (e *Engine) reportUndeclared(server string, names []string) {
	if len(names) == 0 {
		return
	}
	e.undeclaredMu.Lock()
	defer e.undeclaredMu.Unlock()
	if e.undeclared == nil {
		e.undeclared = map[string][]string{}
	}
	if _, seen := e.undeclared[server]; seen {
		return // already logged for this server; a toolset re-binds every turn
	}
	e.undeclared[server] = names
	where := server
	if where == "" {
		where = "（内置）"
	}
	slog.Warn("以下工具没有元数据，按合成的默认值治理",
		"server", where, "tools", names, "count", len(names))
}

// Undeclared returns the tools running on synthesized metadata, by server.
func (e *Engine) Undeclared() map[string][]string {
	e.undeclaredMu.Lock()
	defer e.undeclaredMu.Unlock()
	out := make(map[string][]string, len(e.undeclared))
	for k, v := range e.undeclared {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// ToolRegistry exposes the current registry snapshot, for the API's pre-save
// conflict check and for health output.
// ToolGateway is the gateway every tool call goes through.
//
// Exported for the console, which needs to be able to check that a change it
// made is one the gateway will act on — not merely one the page can draw.
func (e *Engine) ToolGateway() *gateway.Gateway {
	_, gw := e.toolRegistry()
	return gw
}

func (e *Engine) ToolRegistry() *toolreg.Registry {
	store, _ := e.toolRegistry()
	return store.Load()
}

// Metrics returns the shared tool-call tracker, opening the store on first use.
//
// A store that cannot be opened is not an error the caller has to handle: the
// tracker still times calls and simply drops the rows. Losing telemetry must
// never cost a tool call.
func (e *Engine) Metrics() *jellymetrics.Tracker {
	e.metricsOnce.Do(func() {
		dbPath, err := jellysession.DefaultDBPath()
		if err != nil {
			slog.Warn("无法解析指标库路径，埋点已关闭", logging.Err(err))
			e.metrics = jellymetrics.NewTracker(nil)
			return
		}
		rec, err := jellymetrics.NewRecorder(dbPath)
		if err != nil {
			slog.Warn("无法打开指标存储，埋点已关闭", logging.Err(err))
			rec = nil
		}
		e.metrics = jellymetrics.NewTracker(rec)
	})
	return e.metrics
}

// SetMetrics installs a tracker, replacing the store this engine would open on
// its own. It must be called before the first Metrics call.
//
// Tests need this because the default path is the shared ~/.jelly-agent/state.db:
// without an injection point, exercising any handler that reports telemetry
// would write rows into the developer's real database.
func (e *Engine) SetMetrics(tr *jellymetrics.Tracker) {
	e.metricsOnce.Do(func() {}) // claim the once, so lazy init cannot overwrite
	e.metrics = tr
}

// toolCallbacks adapts the tracker to ADK's tool hooks.
//
// Both callbacks return (nil, nil) unconditionally, and that is the whole
// contract here: ADK treats a non-nil return from BeforeToolCallback as "skip
// the tool, use this result", and from AfterToolCallback as "replace the tool's
// output". A measurement hook that ever returns a value would silently swallow
// real tool calls — the kind of bug that looks like a model regression. When
// this seam later grows de-duplication or result shaping, those must be
// separate callbacks with their own tests, not extra branches in these two.
func (e *Engine) toolCallbacks() ([]llmagent.BeforeToolCallback, []llmagent.AfterToolCallback) {
	tr := e.Metrics()

	// These callbacks now only cover tools the gateway does not: an MCP tool
	// or anything without metadata still reaches ADK directly, and a call with
	// no record at all would be worse than one recorded from the model's own
	// arguments. A gatewayed tool is skipped here, because the gateway's sink
	// already wrote the authoritative row — recording both would double every
	// count, and the less accurate row could win a tie.
	isGatewayed := func(t adktool.Tool) bool {
		_, wrapped := t.(*gateway.Wrapped)
		return wrapped
	}

	before := func(ctx agent.ToolContext, t adktool.Tool, args map[string]any) (map[string]any, error) {
		if isGatewayed(t) {
			return nil, nil
		}
		tr.Start(ctx.FunctionCallID(), t.Name(), args)
		return nil, nil
	}
	after := func(ctx agent.ToolContext, t adktool.Tool, args, result map[string]any, err error) (map[string]any, error) {
		if isGatewayed(t) {
			return nil, nil
		}
		row := tr.Finish(jellymetrics.CallMeta{
			SessionID:    ctx.SessionID(),
			InvocationID: ctx.InvocationID(),
			Agent:        ctx.AgentName(),
			CallID:       ctx.FunctionCallID(),
			Tool:         t.Name(),
		}, result, err)
		jellytelemetry.RecordToolCall(ctx, row.Tool, row.OK, string(row.ErrKind), row.Duration)
		return nil, nil
	}
	return []llmagent.BeforeToolCallback{before}, []llmagent.AfterToolCallback{after}
}

// modelCallbacks time model calls and publish their token usage.
//
// Same invariant as the tool hooks and for the same reason: ADK treats a
// non-nil return from either as "use this response instead", so a measurement
// hook that returns a value replaces the model's answer with nothing.
//
// Token counts come from the response's usage metadata, which is also what
// ADK's generate_content span reports — this publishes them as a counter so a
// dashboard can show spend over a window, which a span cannot.
func (e *Engine) modelCallbacks(modelName string, contextWindow, replyTokens int) ([]llmagent.BeforeModelCallback, []llmagent.AfterModelCallback) {
	before := func(ctx agent.CallbackContext, req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
		jellytelemetry.StartLLMCall(ctx.InvocationID())
		// The request about to be sent is exactly the baseline this round's
		// tool results will be appended to, so this is where the room left
		// for them is known. Measured here rather than guessed at from a
		// turn counter, which is what a fixed byte ceiling amounts to.
		e.resultBudget().observe(ctx.InvocationID(),
			promptTokensOf(requestTexts(req)), contextWindow, replyTokens)
		return nil, nil
	}
	after := func(ctx agent.CallbackContext, resp *adkmodel.LLMResponse, respErr error) (*adkmodel.LLMResponse, error) {
		var in, out int64
		name := modelName
		if resp != nil {
			if u := resp.UsageMetadata; u != nil {
				in, out = int64(u.PromptTokenCount), int64(u.CandidatesTokenCount)
			}
			if resp.ModelVersion != "" {
				// Prefer what the provider actually served: a config may name
				// an alias that resolves to a different snapshot.
				name = resp.ModelVersion
			}
		}
		jellytelemetry.RecordLLMCall(ctx, ctx.InvocationID(), name, respErr == nil, in, out)
		return nil, nil
	}
	return []llmagent.BeforeModelCallback{before}, []llmagent.AfterModelCallback{after}
}

// Tools builds the built-in tool set: web_search always, the L1 core tools when
// core is non-nil, and load_memory when withSearch is true.
func (e *Engine) Tools(core *memory.Core, withSearch bool) ([]adktool.Tool, error) {
	tools, err := jellytool.Builtins(core, withSearch)
	if err != nil {
		return nil, err
	}
	// read_result closes the loop the delivery store opened: a large result is
	// bounded before it reaches the prompt, and this is how the rest of it
	// comes back. Offered only when there is somewhere to read from — a tool
	// that always fails is worse than an absent one, because the model spends
	// a turn discovering that.
	if store, serr := e.records(); serr == nil {
		scope := func(tc adktool.Context) record.Scope {
			// The scope comes from the invocation, never from an argument: a
			// handle must not be able to name another conversation's data.
			return record.Scope{AppName: AppName, UserID: UserID, SessionID: tc.SessionID()}
		}
		rr, rerr := jellytool.NewReadResultTool(store, scope)
		if rerr != nil {
			return nil, fmt.Errorf("build read_result: %w", rerr)
		}
		sr, rerr := jellytool.NewSearchResultTool(store, scope)
		if rerr != nil {
			return nil, fmt.Errorf("build search_result: %w", rerr)
		}
		tools = append(tools, rr, sr)
	} else {
		slog.Warn("结果存储不可用，read_result / search_result 未启用（被裁剪的工具返回将无法找回）",
			logging.Err(serr))
	}
	return tools, nil
}

// Skills opens the Agent Skills store from config (or its default dir).
func (e *Engine) Skills() (*skill.Store, error) {
	return skill.NewStore(e.cfg.Skills.Dir)
}

// sandboxPolicy translates the config's sandbox section into a sandbox.Policy
// for script execution. Zero fields keep the sandbox package's own defaults.
func (e *Engine) sandboxPolicy() sandbox.Policy {
	sb := e.cfg.Sandbox
	p := sandbox.Policy{
		Backend:     sb.Backend,
		AllowDocker: sb.AllowDocker,
		Network:     sb.Network,
		Image:       sb.Image,
		CPUSeconds:  sb.CPUSeconds,
		MaxProcs:    sb.MaxProcs,
		MemoryMB:    sb.MemoryMB,
	}
	if sb.TimeoutSec > 0 {
		p.Timeout = time.Duration(sb.TimeoutSec) * time.Second
	}
	if sb.MaxOutputKB > 0 {
		p.MaxOutput = sb.MaxOutputKB << 10
	}
	return p
}

// maxAgentDepth bounds recursion when assembling a coordinator/sub-agent tree,
// a backstop against a misconfigured cycle that path-based detection misses.
const maxAgentDepth = 8

// HasAgents reports whether any named agent is defined in config (multi-agent
// mode). When false the engine builds the legacy single "root" agent.
func (e *Engine) HasAgents() bool {
	for _, a := range e.cfg.Agents {
		if a.Enabled {
			return true
		}
	}
	return false
}

// DefaultAgentName returns the configured default agent name (or "" when none /
// unset / disabled), so callers can pick a root when the request omits one.
func (e *Engine) DefaultAgentName() string {
	if e.cfg.DefaultAgent != "" {
		if def, ok := e.agentDef(e.cfg.DefaultAgent); ok && def.Enabled {
			return e.cfg.DefaultAgent
		}
	}
	for _, a := range e.cfg.Agents { // fall back to the first enabled agent
		if a.Enabled {
			return a.Name
		}
	}
	return ""
}

func (e *Engine) agentDef(name string) (config.AgentDef, bool) {
	for _, a := range e.cfg.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return config.AgentDef{}, false
}

// BuildAgent constructs the root agent for the named provider (empty = default)
// loading every enabled MCP server. See BuildAgentWith for selective MCP.
func (e *Engine) BuildAgent(provider string) (agent.Agent, config.Provider, *memory.Core, *memory.Search, error) {
	return e.BuildAgentWith(provider, nil)
}

// BuildAgentByName assembles the coordinator/sub-agent tree rooted at the named
// agent (delegation via ADK's transfer_to_agent). Each node uses its own
// provider/instruction/MCP; sub-agents are attached recursively with cycle and
// depth guards. Returns the root agent plus the resolved root provider, the
// shared core store and the search service (nil when L2 is off). The caller owns
// search.Close.
func (e *Engine) BuildAgentByName(name string) (agent.Agent, config.Provider, *memory.Core, *memory.Search, error) {
	core, err := e.Core()
	if err != nil {
		return nil, config.Provider{}, nil, nil, fmt.Errorf("init memory: %w", err)
	}
	search, err := e.Search()
	if err != nil {
		return nil, config.Provider{}, nil, nil, err
	}
	a, prov, err := e.buildAgentTree(name, core, search != nil, map[string]bool{}, 0)
	if err != nil {
		if search != nil {
			search.Close()
		}
		return nil, prov, nil, nil, err
	}
	return a, prov, core, search, nil
}

// buildAgentTree recursively builds the agent rooted at name. visited is the
// current path (cycle detection); diamonds (a child shared by two parents) build
// distinct instances, which is fine.
func (e *Engine) buildAgentTree(name string, core *memory.Core, withSearch bool, visited map[string]bool, depth int) (agent.Agent, config.Provider, error) {
	if depth > maxAgentDepth {
		return nil, config.Provider{}, fmt.Errorf("agent 树过深（>%d）：%q 处疑似存在环", maxAgentDepth, name)
	}
	if visited[name] {
		return nil, config.Provider{}, fmt.Errorf("agent 转交存在环：%q", name)
	}
	def, ok := e.agentDef(name)
	if !ok {
		return nil, config.Provider{}, fmt.Errorf("agent %q 未定义", name)
	}
	if !def.Enabled {
		return nil, config.Provider{}, fmt.Errorf("agent %q 已禁用", name)
	}
	visited[name] = true
	defer delete(visited, name)

	var subs []agent.Agent
	for _, child := range def.SubAgents {
		sub, _, err := e.buildAgentTree(child, core, withSearch, visited, depth+1)
		if err != nil {
			return nil, config.Provider{}, fmt.Errorf("构建子 agent %q: %w", child, err)
		}
		subs = append(subs, sub)
	}

	instruction := def.Instruction
	if strings.TrimSpace(instruction) == "" {
		instruction = RootInstruction
	}
	desc := def.Description
	if desc == "" {
		desc = "jelly-agent agent " + name
	}
	// Named agents load only the MCP servers they list (empty ⇒ none), matching
	// PlatformBot semantics — explicit selection in the UI.
	toolsets := e.ToolsetsFor(def.MCP)
	return e.buildNode(name, desc, def.Provider, instruction, toolsets, subs, core, withSearch)
}

// BuildAgentWith is like BuildAgent but controls which MCP servers are loaded:
// nil mcpNames loads all enabled servers (the default), a non-nil slice loads
// only those named (an empty slice loads none). Returns the resolved provider
// plus the core store and search service so the caller can render memory and
// index turns. search is nil when L2 is disabled.
func (e *Engine) BuildAgentWith(provider string, mcpNames []string) (agent.Agent, config.Provider, *memory.Core, *memory.Search, error) {
	core, err := e.Core()
	if err != nil {
		return nil, config.Provider{}, nil, nil, fmt.Errorf("init memory: %w", err)
	}
	search, err := e.Search()
	if err != nil {
		return nil, config.Provider{}, nil, nil, err
	}

	toolsets := e.Toolsets() // nil mcpNames → all enabled MCP servers
	if mcpNames != nil {
		toolsets = e.ToolsetsFor(mcpNames) // selected subset (may be empty)
	}

	a, prov, err := e.buildNode("root", "jelly-agent root agent with web search and core memory.",
		provider, RootInstruction, toolsets, nil, core, search != nil)
	if err != nil {
		if search != nil {
			search.Close()
		}
		return nil, prov, nil, nil, err
	}
	return a, prov, core, search, nil
}

// historyShare is the fraction of a model's context window the conversation
// may occupy when no explicit budget is configured.
//
// The rest goes to the system instruction, the tool schemas and the reply. It
// is deliberately generous rather than thrifty, because compaction is not
// free in the way a token count suggests: every time it fires it rewrites the
// prompt prefix, and a rewritten prefix forfeits the provider's cache on the
// whole history behind it. A tight budget therefore trades a cheap cache read
// for expensive fresh input on every turn, which can cost more than the
// history it trimmed. Anthropic's own server-side compaction triggers at
// roughly 15% of a 1M window for the same reason.
const historyShare = 0.6

// historyBudgetFor resolves the conversation budget.
//
// An explicit setting wins: it is the operator's number and they may have a
// reason this code cannot see. Otherwise it is derived from what the provider
// says its model accepts. Zero means neither is available, which leaves the
// history package's own default to apply — a fallback rather than a guess,
// because a wrong window is worse than an admittedly generic one.
//
// The one explicit value that cannot be meant is a budget larger than the
// window. It does not say "compact later"; it says "never compact", and the
// conversation then grows until the provider rejects the request outright.
// "Never compact" is already expressible as an explicit zero, so a number
// above the window is a mistake rather than a preference — and a silent one,
// because it only shows up as a 400 on a long session. Found in a real
// deployment: history.max_tokens was ten million against a one-million window.
func (e *Engine) historyBudgetFor(contextWindow int) int {
	derived := 0
	if contextWindow > 0 {
		derived = int(float64(contextWindow) * historyShare)
	}
	if n := e.cfg.History.MaxTokens; n != nil {
		if *n > 0 && contextWindow > 0 && *n > contextWindow {
			slog.Warn("history.max_tokens 超过模型上下文窗口，已按窗口比例改用推导值——历史将永不压缩，长会话会被 provider 直接拒绝；请删掉这个设置，或改成不超过窗口的值（显式 0 才表示关闭压缩）",
				"history.max_tokens", *n, "context_window", contextWindow, "改用", derived)
			return derived
		}
		return *n
	}
	return derived
}

// withCompaction layers conversation compaction over the raw LLM so a long
// session (or a few large tool results) can't overrun the context window. An
// explicit history.max_tokens of 0 opts out and returns the model untouched.
func (e *Engine) withCompaction(llm adkmodel.LLM, agentName string, canRecall bool, contextWindow int) adkmodel.LLM {
	h := e.cfg.History
	if h.MaxTokens != nil && *h.MaxTokens <= 0 {
		return llm
	}
	pol := history.Policy{
		KeepRecent:       h.KeepRecent,
		ToolResultTokens: h.ToolResultTokens,
		CanRecall:        canRecall,
		MaxTokens:        e.historyBudgetFor(contextWindow),
	}
	return history.Wrap(llm, pol, func(ctx context.Context, req *adkmodel.LLMRequest, r history.Result) {
		// The trace gets every request; the log gets only the ones where
		// something was actually dropped. A line per turn saying "nothing was
		// compacted" is noise on a terminal and useful on a span.
		var cfg *genai.GenerateContentConfig
		if req != nil {
			cfg = req.Config
		}
		sysTokens, toolTokens, toolCount := jellytelemetry.EstimateConfigTokens(cfg)
		jellytelemetry.RecordPrompt(ctx, jellytelemetry.PromptComposition{
			HistoryTokens:   r.BeforeTokens,
			TokensAfter:     r.AfterTokens,
			ToolsTokens:     toolTokens,
			SystemTokens:    sysTokens,
			ToolCount:       toolCount,
			DroppedContents: r.Dropped,
			TruncatedTools:  r.Truncated,
		})
		if r.Changed() {
			// Context carries the active span, so this line lands on the same
			// trace as the model call it shortened.
			slog.InfoContext(ctx, "上下文压缩",
				"agent", agentName,
				"tokens_before", r.BeforeTokens, "tokens_after", r.AfterTokens,
				"dropped", r.Dropped, "truncated", r.Truncated)
		}
	})
}

// buildNode constructs a single llmagent: it resolves the provider's model,
// assembles the built-in + skill tools, and attaches the given MCP toolsets and
// sub-agents (which give ADK its transfer_to_agent delegation). The instruction
// is rendered fresh each turn (core memory + skill catalog prepended) via an
// InstructionProvider. Shared by the legacy single agent and the multi-agent
// tree so both behave identically.
func (e *Engine) buildNode(name, description, provider, instruction string, toolsets []NamedToolset, subAgents []agent.Agent, core *memory.Core, withSearch bool) (agent.Agent, config.Provider, error) {
	llm, prov, err := e.reg.Get(provider)
	if err != nil {
		return nil, prov, err
	}
	// withSearch is exactly "the agent has load_memory", which decides whether
	// the compaction notice may point the model at it.
	mdl := e.withCompaction(llm, name, withSearch, prov.ContextWindow)

	tools, err := e.Tools(core, withSearch)
	if err != nil {
		return nil, prov, fmt.Errorf("build tools: %w", err)
	}
	tools = append(tools, e.extraTools...)

	// Agent Skills: when any skill is enabled, add the use_skill tool so the
	// agent can pull a skill's full body on demand. The catalog itself is
	// injected per-turn by the InstructionProvider below (read fresh, so edits
	// apply immediately without a rebuild). When script execution is enabled,
	// also expose run_script (with per-skill variables as its environment).
	allowScripts := e.cfg.Skills.AllowScripts
	varsFor := func(name string) map[string]string { return e.cfg.SkillVars[name] }
	if skills, err := e.Skills(); err == nil {
		if cat, err := skills.Catalog(); err == nil && cat != "" {
			if st, err := jellytool.SkillTool(skills, varsFor, allowScripts); err == nil {
				tools = append(tools, st)
			}
			if allowScripts {
				if rs, err := jellytool.RunScriptTool(skills, varsFor, e.sandboxPolicy()); err == nil {
					tools = append(tools, rs)
				}
			}
		}
	}

	// Route every tool through the gateway: our name and schema, the
	// incident's arguments, a shaped result, and evidence the conclusion can
	// cite.
	//
	// MCP toolsets included, which is the point — a third-party server is
	// exactly the thing whose calls nobody could otherwise account for. Their
	// tools are fetched per turn (set.Tools(ctx)) rather than at build time,
	// so they are bound through a toolset wrapper instead of a tool one.
	store, gw := e.toolRegistry()
	binder := &gateway.Binder{
		GW:       gw,
		Registry: gateway.Snapshot(store.Load()),
		Context:  e.incidentFor,
		Fallback: e.undeclaredFallback(),
		Report:   e.reportUndeclared,
	}
	tools = binder.Tools("", tools)
	bound := make([]namedSet, 0, len(toolsets))
	for _, ts := range toolsets {
		bound = append(bound, namedSet{name: ts.Name, set: binder.Toolset(ts.Name, ts.Set)})
	}
	// One selecting toolset rather than a static tool list plus N toolsets:
	// the budget is global, and ADK only re-consults toolsets. See
	// selectingToolset.
	sel := &selectingToolset{
		static:   tools,
		sets:     bound,
		cfg:      selector.Config{MaxTools: e.maxTools()},
		report:   logSelection,
		admit:    e.admissions(),
		health:   e.toolsetHealth(),
		inflight: e.toolListing(),
	}

	beforeTool, afterTool := e.toolCallbacks()
	beforeModel, afterModel := e.modelCallbacks(mdl.Name(), prov.ContextWindow, prov.MaxTokens)

	a, err := llmagent.New(llmagent.Config{
		Name:        name,
		Model:       mdl,
		Description: description,
		// InstructionProvider (not Instruction) so MEMORY.md/USER.md (and the
		// skill catalog) are read fresh each turn. Note: ADK then skips {}
		// session-state substitution.
		InstructionProvider: func(agent.ReadonlyContext) (string, error) {
			return e.systemInstruction(core, instruction, allowScripts), nil
		},
		// Tools is left empty on purpose: a static list is expanded once and
		// never revisited, so anything in it would escape selection.
		Toolsets:  []adktool.Toolset{sel},
		SubAgents: subAgents, // delegation targets (transfer_to_agent), nil for a leaf

		// Telemetry only — see toolCallbacks for why these must not return a
		// value. OnToolErrorCallbacks is deliberately left unset: ADK calls
		// AfterToolCallbacks for failures too, and hooking both would double
		// count every error.
		BeforeToolCallbacks:  beforeTool,
		AfterToolCallbacks:   afterTool,
		BeforeModelCallbacks: beforeModel,
		AfterModelCallbacks:  afterModel,
	})
	if err != nil {
		return nil, prov, fmt.Errorf("create agent: %w", err)
	}
	return a, prov, nil
}

// SetSessionDBPath points the session store somewhere other than the shared
// default.
//
// Tests need this for the same reason they need SetMetrics: without it, any
// handler that lists sessions reads the developer's real ~/.jelly-agent/state.db,
// so the suite's results depend on what that developer happens to have chatted
// about. Call it before the first NewSessionService.
func (e *Engine) SetSessionDBPath(path string) { e.sessionDBPath = path }

// buildRegistry merges the metadata sources into a registry.
//
// A metadata problem is logged and tolerated: an unparsable overlay leaves the
// built-in defaults in place, because refusing to start over a malformed
// description would be a worse outcome than running with fewer wrapped tools.
func buildRegistry(sources []toolreg.Source) *toolreg.Registry {
	metas, err := toolreg.Merge(context.Background(), sources...)
	if err != nil {
		slog.Error("工具元数据加载失败，仅使用内置默认值", logging.Err(err))
		metas, _ = jellytool.BuiltinMetadata().Load(context.Background())
	}
	reg, conflicts := toolreg.Build(metas)
	for _, c := range conflicts {
		// Named on both sides, unlike ADK's own "duplicate tool" — the point
		// of catching this here is that someone can act on it.
		slog.Error("工具注册冲突，该条目未生效", "detail", c.Error())
	}
	return reg
}

// ReloadToolMetadata re-reads the declarations and swaps the registry.
//
// Deliberately not a config reload. Rebuilding the engine cancels its MCP
// context, which terminates every stdio subprocess and drops every open
// session — an acceptable price for changing a provider, an absurd one for
// saying that a tool returns metrics. The registry was always a swappable
// store; this is what it was for.
func (e *Engine) ReloadToolMetadata() {
	store, _ := e.toolRegistry() // ensure it exists before swapping into it
	sources := []toolreg.Source{jellytool.BuiltinMetadata()}
	if dir := e.ToolMetadataDir(); dir != "" {
		sources = append(sources, toolreg.NewFileSource(dir))
	}
	store.Swap(buildRegistry(sources))
}

// ToolMetadataDir is where this deployment's tool declarations live.
//
// Resolved rather than read straight off the config, so the default applies
// and every caller — the loader, the console, the writer — agrees on one
// location.
func (e *Engine) ToolMetadataDir() string {
	return config.ToolMetadataDir(e.cfg, e.cfg.SourcePath)
}

// SessionDBPath is where the session store lives, for the handlers that query
// it directly rather than through the ADK service.
//
// Empty means the shared default, which is what the session package resolves
// for itself — so an empty return is a valid argument, not a missing one. It
// is exported because two handlers were passing "" unconditionally and thereby
// ignoring the override entirely: harmless in a deployment, where the override
// is unset, and wrong everywhere else.
func (e *Engine) SessionDBPath() string { return e.sessionDBPath }

// NewSessionService opens the persistent SQLite session store. The CLI and web
// server share one store, so history is consistent across both front ends.
func (e *Engine) NewSessionService() (adksession.Service, error) {
	return jellysession.NewSQLite(e.sessionDBPath)
}

// NewRunner builds a runner backed by the persistent SQLite session store,
// wiring search as the MemoryService when non-nil (PLAN §10.1).
func (e *Engine) NewRunner(a agent.Agent, search *memory.Search) (*runner.Runner, adksession.Service, error) {
	svc, err := e.NewSessionService()
	if err != nil {
		return nil, nil, err
	}
	cfg := runner.Config{
		AppName:        AppName,
		Agent:          a,
		SessionService: svc,
	}
	if search != nil {
		cfg.MemoryService = search
	}
	r, err := runner.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create runner: %w", err)
	}
	return r, svc, nil
}
