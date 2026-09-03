// Package engine assembles jelly-agent's runtime — model, agent, runner,
// session store, and memory layers — from a loaded config. It is the single
// source of truth shared by the CLI (cmd/cli) and the web server (cmd/server),
// so both drive the same agent the same way.
package engine

import (
	"context"
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
	"github.com/jelly-agent/jelly-agent/internal/sandbox"
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
	// unregistered names the tools no metadata covers, reported once at
	// startup rather than left invisible.
	unregisteredOnce sync.Once
	unregistered     []string
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

// Toolsets returns every enabled MCP toolset (used by the web chat / default
// agent, which loads all of them).
func (e *Engine) Toolsets() []adktool.Toolset {
	e.buildToolsets()
	out := make([]adktool.Toolset, 0, len(e.mcpSets))
	for _, srv := range e.cfg.MCP { // stable order = config order
		if ts, ok := e.mcpSets[srv.Name]; ok {
			out = append(out, ts)
		}
	}
	return out
}

// ToolsetsFor returns only the named enabled MCP toolsets, in the given order —
// the basis for a bot loading a selected subset of MCP servers.
func (e *Engine) ToolsetsFor(names []string) []adktool.Toolset {
	e.buildToolsets()
	out := make([]adktool.Toolset, 0, len(names))
	for _, n := range names {
		if ts, ok := e.mcpSets[n]; ok {
			out = append(out, ts)
		}
	}
	return out
}

// SearchEnabled reports whether L2 session search is turned on in config.
func (e *Engine) SearchEnabled() bool { return e.cfg.Memory.Search.Enabled }

// Core builds the L1 core-memory store from config (or its defaults).
func (e *Engine) Core() (*memory.Core, error) {
	mc := e.cfg.Memory.Core
	return memory.NewCore(mc.Dir, mc.MemoryBudgetTokens, mc.UserBudgetTokens)
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
		if dir := e.cfg.Tools.MetadataDir; dir != "" {
			sources = append(sources, toolreg.NewFileSource(dir))
		}
		metas, err := toolreg.Merge(context.Background(), sources...)
		if err != nil {
			slog.Error("工具元数据加载失败，仅使用内置默认值", logging.Err(err))
			metas, _ = jellytool.BuiltinMetadata().Load(context.Background())
		}
		reg, conflicts := toolreg.Build(metas)
		for _, c := range conflicts {
			// Named on both sides, unlike ADK's own "duplicate tool" — the
			// point of catching this here is that someone can act on it.
			slog.Error("工具注册冲突，该条目未生效", "detail", c.Error())
		}
		e.toolStore.Swap(reg)

		// The ceiling is the widest level any built-in declares, because this
		// change must not take away a tool that works today: remember and
		// forget write local memory, and run_script runs sandboxed code. A
		// narrower ceiling here would silently disable them — a regression
		// dressed up as a safety improvement.
		//
		// Tightening this is the second half of the work, and it needs an
		// approval path behind it (ADK's RequestConfirmation into a DingTalk
		// card) rather than a smaller constant. The per-tool levels are
		// already recorded, so that change becomes a policy edit.
		e.gw = gateway.New(gateway.Config{
			Registry: gateway.Snapshot(reg),
			Policy: gateway.Policy{
				MaxSideEffect:         ops.SideEffectRisky,
				AllowApprovalRequired: true,
			},
		})
	})
	return e.toolStore, e.gw
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

// reportUnregistered logs the tools no metadata covers, once.
//
// Reported rather than silent because an unwrapped tool bypasses the gateway
// entirely — no argument injection, no shaping, no evidence — and that is
// exactly the kind of gap that is invisible until someone wonders why one
// tool's results are shaped and another's are not.
func (e *Engine) reportUnregistered(names []string) {
	if len(names) == 0 {
		return
	}
	e.unregisteredOnce.Do(func() {
		e.unregistered = names
		slog.Warn("以下工具没有元数据，未经过 Gateway（无参数注入与结果整形）",
			"tools", names, "count", len(names))
	})
}

// ToolRegistry exposes the current registry snapshot, for the API's pre-save
// conflict check and for health output.
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

	before := func(ctx agent.ToolContext, t adktool.Tool, args map[string]any) (map[string]any, error) {
		tr.Start(ctx.FunctionCallID(), t.Name(), args)
		return nil, nil
	}
	after := func(ctx agent.ToolContext, t adktool.Tool, args, result map[string]any, err error) (map[string]any, error) {
		row := tr.Finish(jellymetrics.CallMeta{
			SessionID:    ctx.SessionID(),
			InvocationID: ctx.InvocationID(),
			Agent:        ctx.AgentName(),
			CallID:       ctx.FunctionCallID(),
			Tool:         t.Name(),
		}, result, err)
		// The same judgement, published twice for two different questions: the
		// row answers "what did this task do", the metric answers "how is this
		// tool trending". Finish already decided success and cause, so the
		// metric cannot disagree with the table.
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
func (e *Engine) modelCallbacks(modelName string) ([]llmagent.BeforeModelCallback, []llmagent.AfterModelCallback) {
	before := func(ctx agent.CallbackContext, req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
		jellytelemetry.StartLLMCall(ctx.InvocationID())
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
	return jellytool.Builtins(core, withSearch)
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

// withCompaction layers conversation compaction over the raw LLM so a long
// session (or a few large tool results) can't overrun the context window. An
// explicit history.max_tokens of 0 opts out and returns the model untouched.
func (e *Engine) withCompaction(llm adkmodel.LLM, agentName string, canRecall bool) adkmodel.LLM {
	h := e.cfg.History
	if h.MaxTokens != nil && *h.MaxTokens <= 0 {
		return llm
	}
	pol := history.Policy{KeepRecent: h.KeepRecent, ToolResultTokens: h.ToolResultTokens, CanRecall: canRecall}
	if h.MaxTokens != nil {
		pol.MaxTokens = *h.MaxTokens
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
func (e *Engine) buildNode(name, description, provider, instruction string, toolsets []adktool.Toolset, subAgents []agent.Agent, core *memory.Core, withSearch bool) (agent.Agent, config.Provider, error) {
	llm, prov, err := e.reg.Get(provider)
	if err != nil {
		return nil, prov, err
	}
	// withSearch is exactly "the agent has load_memory", which decides whether
	// the compaction notice may point the model at it.
	mdl := e.withCompaction(llm, name, withSearch)

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

	// Route the built-in tools through the gateway: our name and schema, the
	// incident's arguments, a shaped result, and evidence the conclusion can
	// cite.
	//
	// MCP toolsets are deliberately left on the direct path for now. A
	// toolset's tools are fetched per turn (set.Tools(ctx)), not at build
	// time, so wrapping them needs a Toolset wrapper rather than a tool one —
	// a separate change, and one worth making with a real MCP server in hand
	// to test against.
	store, gw := e.toolRegistry()
	snapshot := gateway.Snapshot(store.Load())
	gw.SetExecutor("", gateway.InnerExecutor(tools))
	tools, unregistered := gateway.WrapTools(gw, snapshot, "", e.incidentFor, tools)
	e.reportUnregistered(unregistered)

	beforeTool, afterTool := e.toolCallbacks()
	beforeModel, afterModel := e.modelCallbacks(mdl.Name())

	a, err := llmagent.New(llmagent.Config{
		Name:        name,
		Model:       mdl,
		Description: description,
		// InstructionProvider (not Instruction) so MEMORY.md/USER.md (and the
		// skill catalog) are read fresh each turn. Note: ADK then skips {}
		// session-state substitution.
		InstructionProvider: func(agent.ReadonlyContext) (string, error) {
			base := core.Render(instruction)
			if skills, err := e.Skills(); err == nil {
				if cat, err := skills.Catalog(); err == nil && cat != "" {
					base += "\n\n" + cat
					if allowScripts {
						base += "目录型技能可能附带脚本；需要时用 run_script 运行（凭据已由系统注入环境变量，按 use_skill 给出的 var_keys 在脚本里引用，切勿向用户索要密钥）。\n"
					}
				}
			}
			return base, nil
		},
		Tools:     tools,
		Toolsets:  toolsets,  // external MCP servers (all enabled, or a selected subset)
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
