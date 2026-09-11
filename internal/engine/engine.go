// Package engine assembles jelly-agent's runtime — model, agent, runner,
// session store, and memory layers — from a loaded config. It is the single
// source of truth shared by the CLI (cmd/cli) and the web server (cmd/server),
// so both drive the same agent the same way.
package engine

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/jelly-agent/jelly-agent/internal/tokens"
	jellytool "github.com/jelly-agent/jelly-agent/internal/tool"
	"github.com/jelly-agent/jelly-agent/internal/toolreg"

	"github.com/jelly-agent/jelly-agent/internal/logging"
	"github.com/jelly-agent/jelly-agent/internal/schedule"
	"github.com/jelly-agent/jelly-agent/internal/storage"
	"github.com/jelly-agent/jelly-agent/internal/task"
	"path/filepath"
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

	// RootInstruction is the built-in base instruction, used when the config
	// does not supply one. L1 core memory is prepended to it each turn via the
	// InstructionProvider (PLAN §10.1). Read it through BaseInstruction, never
	// directly — the whole point of the config field is that a deployment can
	// say what it actually is instead of "一个用 Go + ADK-Go 构建的助手".
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

	// stateRef overrides the shared store's location. Empty means the
	// default path; only tests and embedders set it.
	stateRef string

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

	// intentOnce/intent remember what each conversation has been asking for,
	// so a follow-up that names no metric does not lose the metric tools.
	intentOnce sync.Once
	intent     *intents

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

	// stateMu guards the one handle on the shared state database.
	//
	// Four stores used to open their own on every call and close it again —
	// free against a local SQLite file, a TCP and authentication handshake
	// against PostgreSQL. Measured at 9.3ms per call versus 579µs on a handle
	// that is kept, so a sessions page paid the 9.3ms before doing any work
	// and a bulk delete paid it three times.
	//
	// A mutex rather than a sync.Once, because a Once remembers the failure
	// too: a database that was unreachable the first time anything asked was
	// unreachable for the rest of the process's life, however long it had
	// been back. Success is what is remembered here; a failure is just this
	// call's answer, and the next caller tries again.
	stateMu     sync.Mutex
	stateDB     *storage.DB
	stateClosed bool

	// sessionOnce guards the ADK session service, which six request paths
	// used to build afresh on every call. Building it opens a connection and
	// runs AutoMigrate — 857µs against a SQLite file and 733ms against
	// PostgreSQL, where the migration inspects the catalogue for four tables
	// over the wire. That was 85% of what a session timeline cost.
	sessionOnce  sync.Once
	sessionSvc   adksession.Service
	sessionClose func() error
	sessionErr   error

	// promptByAgent is the last exact fixed prompt shape observed at the model
	// boundary. The console cannot reconstruct MCP schemas from metadata: the
	// parameter JSON is most of their cost, and undeclared MCP tools are absent
	// from the registry altogether.
	promptMu      sync.RWMutex
	promptByAgent map[string]PromptSnapshot

	// missingRequired tracks required_tools/required_suites entries the
	// catalogue could not offer, so a typo is loud once instead of silent.
	missingMu       sync.Mutex
	missingRequired map[string][]string
}

// PromptToolSnapshot is one declaration exactly as it reached the model.
type PromptToolSnapshot struct {
	Name        string
	Description string
	Tokens      int
	// Server is which MCP server the tool came from, empty for a built-in.
	//
	// Carried because the console has to resolve the tool's side-effect level,
	// and an undeclared remote tool is not in the registry to look up. Absent,
	// the page rendered an empty badge for it — and an empty badge reads as
	// "no side effect", which is the one thing the policy says a third-party
	// server's silence must never be taken for.
	Server string
}

// PromptSnapshot is the latest exact fixed-cost observation for an agent.
//
// The system instruction is deliberately not here: this process assembles it,
// so estimating from our own text is as exact as the model boundary could be,
// and carrying a second number would invite reading it as the measured one.
type PromptSnapshot struct {
	ToolsTokens int
	Tools       []PromptToolSnapshot
	// At is when the request was observed. The page says "measured", and
	// without this it cannot say measured *when* — which matters most right
	// after an operator changes metadata or max_tools, because the newest
	// observation still predates the change until a turn runs.
	At time.Time
}

// SetPromptSnapshotForTest injects an observation without running a turn.
//
// The alternative is a live model call, which is what this observation comes
// from — so the console's side of it would otherwise have no test at all, and
// the field it reads is exactly where a blank side-effect badge came from.
func (e *Engine) SetPromptSnapshotForTest(agent string, p PromptSnapshot) {
	e.promptMu.Lock()
	defer e.promptMu.Unlock()
	if e.promptByAgent == nil {
		e.promptByAgent = map[string]PromptSnapshot{}
	}
	e.promptByAgent[agent] = p
}

// LastPromptSnapshot returns the last request observed for agent.
func (e *Engine) LastPromptSnapshot(agent string) (PromptSnapshot, bool) {
	e.promptMu.RLock()
	defer e.promptMu.RUnlock()
	p, ok := e.promptByAgent[agent]
	p.Tools = append([]PromptToolSnapshot(nil), p.Tools...)
	return p, ok
}

func (e *Engine) observePrompt(agentName string, cfg *genai.GenerateContentConfig, _, toolsTokens int) {
	if cfg == nil {
		return
	}
	// Server attribution comes from the gateway, not the registry: it answers
	// for adopted tools too, which is every MCP tool nobody has declared.
	_, gw := e.toolRegistry()
	var tools []PromptToolSnapshot
	for _, group := range cfg.Tools {
		if group == nil {
			continue
		}
		for _, d := range group.FunctionDeclarations {
			if d == nil {
				continue
			}
			n := tokens.Estimate(d.Name) + tokens.Estimate(d.Description)
			if b, err := json.Marshal(jellymodel.ToolParameters(d)); err == nil {
				n += tokens.Estimate(string(b))
			}
			snap := PromptToolSnapshot{Name: d.Name, Description: d.Description, Tokens: n}
			if m, ok := gw.Metadata(d.Name); ok {
				snap.Server = m.Server
			}
			tools = append(tools, snap)
		}
	}
	e.promptMu.Lock()
	if e.promptByAgent == nil {
		e.promptByAgent = map[string]PromptSnapshot{}
	}
	e.promptByAgent[agentName] = PromptSnapshot{
		ToolsTokens: toolsTokens, Tools: tools, At: time.Now(),
	}
	e.promptMu.Unlock()
}

// New wraps a loaded config in an engine.
func New(cfg *config.Config) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{cfg: cfg, reg: jellymodel.NewRegistry(cfg), mcpCtx: ctx, mcpCancel: cancel}
	// Taken here rather than read at each use, so a hot reload cannot swap the
	// database out from under handles that are already open on the old one.
	// Changing it needs a restart, which is the honest behaviour for the store
	// every other store is keyed against.
	e.stateRef = cfg.Storage.DSN
	return e
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
	// The field, not records(): calling the accessor would open the store in
	// order to close it, and on a config reload that means a fresh connection
	// pool created and abandoned on every reload.
	//
	// Closing it at all is the fix. It was left out, so each reload leaked the
	// pool the previous engine had opened — free against a SQLite file, and on
	// PostgreSQL a slow walk into "too many clients already".
	if e.recordStore != nil {
		if err := e.recordStore.Close(); err != nil {
			slog.Warn("关闭产物存储失败", logging.Err(err))
		}
	}
	// ADK's Service has no Close, so the pool behind it is released through
	// the handle this package opened for it. Without that a config reload
	// abandons one more pool — the same leak the delivery store had, on the
	// connection every page goes through.
	if e.sessionClose != nil {
		if err := e.sessionClose(); err != nil {
			slog.Warn("关闭会话存储失败", logging.Err(err))
		}
	}
	e.stateMu.Lock()
	db := e.stateDB
	e.stateDB, e.stateClosed = nil, true
	e.stateMu.Unlock()
	if db != nil {
		if err := db.Close(); err != nil {
			slog.Warn("关闭状态数据库失败", logging.Err(err))
		}
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
	dbPath, err := e.stateReference()
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

		// The file layer is always consulted, because the directory now has a
		// default: declarations are how an MCP tool says what it produces,
		// and a layer reachable only by setting a path nobody knows about is
		// a layer nobody uses. A missing directory is not an error — it means
		// nothing has been declared yet. See metadataSources for the layering.
		// The overlay is loaded here, by hand, rather than left to
		// metadataSources — and the version that goes with it is read
		// first. Both for the same reason: the version is a claim that the
		// registry has this change in it, and only a load whose result was
		// actually installed can support that claim. A version taken after
		// the load would already cover a change the load missed; a version
		// confirmed against a *different* load than the one installed
		// covers whatever that other load happened to see.
		src, since, overlay := e.loadDeclOverlay()
		reg, complete := buildRegistryOrBuiltins(e.registrySources(overlay))
		e.toolStore.Swap(reg)
		if !complete {
			// The fallback registry does not contain the overlay, so the
			// version it came with is not a claim this process can make. The
			// same rule as the watcher's, at the one build that is not the
			// watcher's: a version is spent by an install, not by a load.
			since = toolreg.NoVersion
		}
		afterFirstRegistry() // a seam; the window this ordering closes
		e.watchDeclarations(src, since)

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
func (e *Engine) SystemPrompt(provider, agent string) (parts []PromptPart, err error) {
	core, err := e.Core()
	if err != nil {
		return nil, err
	}
	mem, user := core.Snapshot()
	allow := e.cfg.Skills.AllowScripts

	// The instruction of the agent being asked about, not the base one.
	//
	// This page answers "what do we inject", and it used to answer for the
	// built-in constant whatever was actually running. A deployment with a
	// coordinator that has its own instruction was shown a prompt the model
	// never receives, under a heading that says 发给模型的原文.
	instruction := e.InstructionFor(agent)
	parts = append(parts, PromptPart{Name: "指令", Text: instruction})
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
	full := e.systemInstruction(core, instruction, allow)
	parts = append(parts, PromptPart{Name: "完整拼装", Text: full, Assembled: true})
	return parts, nil
}

// BaseInstruction is the instruction every agent starts from: the configured
// one, or the built-in default when the config says nothing.
func (e *Engine) BaseInstruction() string {
	if e.cfg != nil {
		if s := strings.TrimSpace(e.cfg.Instruction); s != "" {
			return s
		}
	}
	return RootInstruction
}

// InstructionFor is the instruction a named agent actually runs with: its own
// when it declares one, the base otherwise. An unknown or empty name is
// single-agent mode, which is the base.
func (e *Engine) InstructionFor(agent string) string {
	agent = strings.TrimSpace(agent)
	if agent != "" && e.cfg != nil {
		for _, def := range e.cfg.Agents {
			if def.Name != agent {
				continue
			}
			if s := strings.TrimSpace(def.Instruction); s != "" {
				return s
			}
			break
		}
	}
	return e.BaseInstruction()
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

// StateDB is the process's one handle on the shared state database.
//
// The stores that live in it — sessions, task links, the schedule log, the L2
// memory index — take this rather than a path, so the process holds one pool
// instead of opening one per call. Each store's schema is created here, once,
// for the same reason: the openers this replaced ran CREATE TABLE IF NOT
// EXISTS on every call, which is a no-op that still costs a round trip.
//
// The handle is not closed per use. It lives as long as the process, which is
// what a pool is for.
//
// A failure is not remembered. Opening is retried on the next call, because
// the interesting failure is transient — a database still starting, a network
// that came back — and remembering it meant one unlucky moment at boot left
// the process with no state for as long as it ran: no sessions, no task
// links, and a tool registry permanently missing its console layer, with
// nothing that would ever try again.
func (e *Engine) StateDB() (*storage.DB, error) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.stateDB != nil {
		return e.stateDB, nil
	}
	if e.stateClosed {
		return nil, errEngineClosed
	}
	open := func() (*storage.DB, error) {
		path, err := e.stateReference()
		if err != nil {
			return nil, err
		}
		db, err := storage.Open(path)
		if err != nil {
			return nil, err
		}
		for _, ensure := range []func(*storage.DB) error{
			jellysession.EnsureSchema,
			task.EnsureSchema,
			toolreg.EnsureSchema,
			schedule.EnsureSchema,
			memory.EnsureSchema,
		} {
			if err := ensure(db); err != nil {
				db.Close()
				return nil, err
			}
		}
		return db, nil
	}
	db, err := open()
	if err != nil {
		return nil, err
	}
	e.stateDB = db
	{
		// A deployment upgrading across the move from console.yaml to the
		// table has one, holding decisions somebody made. Once, and only into
		// an empty table — see ImportConsoleFile.
		//
		// Only when the two came from the same place. ToolMetadataDir falls
		// back to ~/.jelly-agent/tools when nothing is configured, so a
		// process pointed at some other database — a test with a temp file,
		// a one-off run against a copy — would otherwise import the real
		// deployment's file into its own database and rename the original out
		// of the way. Reading it there is harmless; renaming it is not, and
		// it happened.
		if dir := e.importableMetadataDir(); dir != "" {
			path := filepath.Join(dir, toolreg.ConsoleFile)
			switch n, err := toolreg.ImportConsoleFile(context.Background(), e.stateDB, path); {
			case err != nil:
				slog.Warn("导入 console.yaml 失败，控制台的旧声明这次没有生效",
					"path", path, logging.Err(err))
			case n > 0:
				slog.Info("已把 console.yaml 导入 tool_decls", "declarations", n,
					"renamed_to", path+".imported")
			}
		}
	}
	return e.stateDB, nil
}

// errEngineClosed is what a closed engine answers, so a retry loop stops
// rather than reopening the database a Close just released.
var errEngineClosed = errors.New("engine: 已关闭")

// records opens the delivery store on first use.
func (e *Engine) records() (*record.Store, error) {
	e.recordsOnce.Do(func() {
		var path string
		if path, e.recordsErr = e.stateReference(); e.recordsErr != nil {
			return
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

// reportMissingRequired surfaces a required tool or suite the catalogue could
// not offer, once per agent per distinct shortfall.
//
// De-duplicated because a toolset re-selects every turn: without it a typo
// would print on every message. Keyed by the shortfall as well as the agent,
// so a server coming back — which changes the shortfall — is reported again
// rather than suppressed by the first one.
func (e *Engine) reportMissingRequired(agent string, missing []string) {
	if len(missing) == 0 {
		return
	}
	key := agent + "\x00" + strings.Join(missing, ",")
	e.missingMu.Lock()
	if e.missingRequired == nil {
		e.missingRequired = map[string][]string{}
	}
	_, seen := e.missingRequired[key]
	e.missingRequired[agent] = missing
	if !seen {
		e.missingRequired[key] = missing
	}
	e.missingMu.Unlock()
	if seen {
		return
	}
	slog.Warn("agent 声明的必需工具本轮拿不到（名字写错、suite 没人声明，或该 MCP 服务器暂时不可达）",
		"agent", agent, "missing", missing, "count", len(missing))
}

// MissingRequired reports, per agent, the required entries last found missing.
func (e *Engine) MissingRequired() map[string][]string {
	e.missingMu.Lock()
	defer e.missingMu.Unlock()
	out := make(map[string][]string, len(e.missingRequired))
	for k, v := range e.missingRequired {
		if strings.Contains(k, "\x00") {
			continue // the de-dup key, not an agent
		}
		out[k] = append([]string(nil), v...)
	}
	return out
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
		// The configured reference, not the default. Call records join to
		// events and tool results on (session_id, invocation_id); writing them
		// to a different database than those makes every join in the console
		// return nothing, on a deployment that otherwise looks fine.
		ref, err := e.stateReference()
		if err != nil {
			slog.Warn("无法解析指标库位置，埋点已关闭", logging.Err(err))
			e.metrics = jellymetrics.NewTracker(nil)
			return
		}
		rec, err := jellymetrics.NewRecorder(ref)
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
		instruction = e.BaseInstruction()
	}
	desc := def.Description
	if desc == "" {
		desc = "jelly-agent agent " + name
	}
	// Named agents load only the MCP servers they list (empty ⇒ none), matching
	// PlatformBot semantics — explicit selection in the UI.
	toolsets := e.ToolsetsFor(def.MCP)
	return e.buildNode(name, desc, def.Provider, instruction, toolsets, subs, core, withSearch,
		def.RequiredTools, def.RequiredSuites)
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
		provider, e.BaseInstruction(), toolsets, nil, core, search != nil, nil, nil)
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
		e.observePrompt(agentName, cfg, sysTokens, toolTokens)
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
func (e *Engine) buildNode(name, description, provider, instruction string, toolsets []NamedToolset, subAgents []agent.Agent, core *memory.Core, withSearch bool, requiredTools, requiredSuites []string) (agent.Agent, config.Provider, error) {
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
		GW: gw,
		// MCP toolsets are rebound on every turn, so use the live registry here:
		// metadata saved from the console must affect selection on the next
		// turn without restarting the agent or its MCP subprocesses.
		Registry: gateway.Live(store),
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
		static: tools,
		sets:   bound,
		cfg: selector.Config{
			MaxTools: e.maxTools(), RequiredTools: requiredTools, RequiredSuites: requiredSuites,
		},
		report:        logSelection,
		agent:         name,
		reportMissing: e.reportMissingRequired,
		admit:         e.admissions(),
		carried:       e.intents(),
		health:        e.toolsetHealth(),
		inflight:      e.toolListing(),
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

// SetStateRef points the state database somewhere other than the shared
// default.
//
// Tests need this for the same reason they need SetMetrics: without it, any
// handler that lists sessions reads the developer's real
// ~/.jelly-agent/state.db, so the suite's results depend on what that
// developer happens to have chatted about. Call it before the first StateDB or
// NewSessionService.
func (e *Engine) SetStateRef(ref string) { e.stateRef = ref }

// buildRegistry merges the metadata sources into a registry.
//
// A metadata problem is logged and tolerated: an unparsable overlay leaves the
// built-in defaults in place, because refusing to start over a malformed
// description would be a worse outcome than running with fewer wrapped tools.
// buildRegistry merges the layers into a registry, or reports why it could
// not.
//
// The error is returned rather than swallowed because the callers want
// different things from it: a startup with an unreadable metadata file is
// better off with built-in defaults than with nothing, while a rebuild
// triggered by a change must not install a registry that is missing layers —
// it would replace a correct one with a worse one and call it done.
func buildRegistry(sources []toolreg.Source) (*toolreg.Registry, error) {
	metas, err := toolreg.Merge(context.Background(), sources...)
	if err != nil {
		return nil, err
	}
	reg, conflicts := toolreg.Build(metas)
	for _, c := range conflicts {
		// Named on both sides, unlike ADK's own "duplicate tool" — the point
		// of catching this here is that someone can act on it.
		slog.Error("工具注册冲突，该条目未生效", "detail", c.Error())
	}
	return reg, nil
}

// buildRegistryOrBuiltins is buildRegistry for the first build, where there
// is no previous registry to keep: a deployment with one unreadable metadata
// file still has to start, and built-in defaults are what it starts with.
//
// The second return says whether the layers it was given are the ones that
// went in. It matters because the caller is about to tell the watcher which
// change the registry already has: a fallback registry has none of them, and
// saying otherwise leaves the declarations out with nothing that would ever
// put them back — the watcher only reacts to the version moving past a
// baseline it has already passed.
func buildRegistryOrBuiltins(sources []toolreg.Source) (*toolreg.Registry, bool) {
	reg, err := buildRegistry(sources)
	if err == nil {
		return reg, true
	}
	slog.Error("工具元数据加载失败，仅使用内置默认值，稍后重试", logging.Err(err))
	metas, _ := jellytool.BuiltinMetadata().Load(context.Background())
	builtins, _ := toolreg.Build(metas)
	return builtins, false
}

// declPoll is how often watchDeclarations asks whether another process
// changed a declaration. A variable so a test does not have to wait it out;
// zero means the source's own default.
var declPoll = 0 * time.Second

func declPollInterval() time.Duration {
	if declPoll > 0 {
		return declPoll
	}
	return toolreg.DefaultPollInterval
}

// watchDeclarations rebuilds the registry when a declaration changes in the
// database, including one this process did not make.
//
// The table was put in the database so several processes would agree about
// what a tool is, and tool_decl_log was made to double as the notice — every
// process polls its highest id and reloads when it moves. That was built and
// then not connected to anything: a save in one console updated that
// process's registry (ReloadToolMetadata, right below) and no other's, so a
// two-process deployment quietly disagreed about tool metadata until someone
// restarted it. This is the consumer.
//
// Started even when the database would not open, and that is the point of it
// being a loop rather than a subscription: StateDB retries per call, but only
// if something calls it, so without this a database that was down for the
// first second of the process left the registry permanently without its
// console layer and nothing that would ever fill it in.
//
// Stops with the engine — the context is the one Close cancels — so a
// replaced engine's watcher goes away with it rather than swapping into a
// registry nobody reads.
func (e *Engine) watchDeclarations(src *toolreg.DBSource, since int64) {
	go func() {
		t := time.NewTicker(declPollInterval())
		defer t.Stop()
		seen := since
		for {
			select {
			case <-e.mcpCtx.Done():
				return
			case <-t.C:
			}
			if src == nil {
				// The database would not open when the registry was first
				// built. Nothing else is going to try again — StateDB is
				// retried per call, but only if something calls it — so the
				// watcher is what makes that a delay instead of a permanent
				// state. Until it opens there is nothing to poll.
				if src, seen = e.declSource(), toolreg.NoVersion; src == nil {
					continue
				}
			}
			overlay, v, changed, err := src.Poll(e.mcpCtx, seen)
			if err != nil || !changed {
				continue
			}
			// The set the poll handed over, not another read of the same
			// table. Reading again meant the version was confirmed by one
			// load and the registry built from a second one: if that second
			// load failed, a registry with no overlay in it went in — every
			// declaration gone, back to built-in defaults — while the change
			// was already recorded as handled.
			//
			// The other layers are files and built-ins; the overlay is what
			// this version tracks, and it is already in hand.
			beforeWatchSwap() // a seam; a test breaks a layer here
			reg, err := buildRegistry(e.registrySources(overlay))
			if err != nil {
				// Not installed, so not handled. The baseline stays where it
				// was and the next tick tries the same change again. A
				// version spent on an install that did not happen is a
				// registry that stays wrong with nothing left to correct it.
				slog.Warn("工具声明变了，但注册表没建起来，下一轮再试", logging.Err(err))
				continue
			}
			e.toolStore.Swap(reg)
			seen = v
			slog.Info("工具声明有变化，已重建注册表")
		}
	}()
}

// declSource opens the declaration source, or nil when the database will not
// open. Separate from loadDeclOverlay because the watcher needs to keep
// trying for one long after startup has given up.
func (e *Engine) declSource() *toolreg.DBSource {
	db, err := e.StateDB()
	if err != nil {
		return nil
	}
	return toolreg.NewDBSource(db).WithPoll(declPollInterval())
}

// loadDeclOverlay opens the declaration source, reads which change it is at,
// and loads it — in that order.
//
// Returns a baseline of toolreg.NoVersion when there is nothing it can vouch for:
// the database will not open, the version will not read, or the load failed.
// The watcher then treats the first tick as a change and loads again, which
// is the retry the old code did not have — a failed load at startup left the
// registry without its overlay and the watcher starting from a version that
// already covered it.
func (e *Engine) loadDeclOverlay() (*toolreg.DBSource, int64, []ops.ToolMetadata) {
	src := e.declSource()
	if src == nil {
		slog.Warn("状态库打不开，工具声明这一层先空着，watcher 会一直重试")
		return nil, toolreg.NoVersion, nil
	}
	v, err := src.Version(e.mcpCtx)
	if err != nil {
		slog.Warn("读不到工具声明的版本号，注册表先按文件层建，稍后补载", logging.Err(err))
		return src, toolreg.NoVersion, nil
	}
	overlay, err := src.Load(e.mcpCtx)
	if err != nil {
		slog.Warn("读不到工具声明，注册表先按文件层建，稍后补载", logging.Err(err))
		return src, toolreg.NoVersion, nil
	}
	return src, v, overlay
}

// registrySources is every layer, with declarations already in hand as the
// overlay. A nil overlay means there is none to apply — not an empty one,
// which would be a claim that nothing is declared.
func (e *Engine) registrySources(overlay []ops.ToolMetadata) []toolreg.Source {
	sources := e.fileMetadataSources()
	if overlay != nil {
		sources = append(sources, toolreg.StaticOverlay{
			StaticSource: toolreg.StaticSource{Label: "db:tool_decls", Metas: overlay},
		})
	}
	return sources
}

// beforeWatchSwap is the moment a delivered change is about to be installed.
// A test makes the database unreadable here, to show that installing it does
// not depend on reading anything. Nothing in production replaces it.
var beforeWatchSwap = func() {}

// afterFirstRegistry is the moment between the registry's first load and the
// watcher starting — the window a change had to land in to be lost. A test
// writes a declaration here; nothing in production replaces it.
var afterFirstRegistry = func() {}

// ReloadToolMetadata re-reads the declarations and swaps the registry.
//
// Deliberately not a config reload. Rebuilding the engine cancels its MCP
// context, which terminates every stdio subprocess and drops every open
// session — an acceptable price for changing a provider, an absurd one for
// saying that a tool returns metrics. The registry was always a swappable
// store; this is what it was for.
func (e *Engine) ReloadToolMetadata() {
	store, _ := e.toolRegistry() // ensure it exists before swapping into it
	reg, err := buildRegistry(e.metadataSources())
	if err != nil {
		// The registry it has is better than one built from fewer layers.
		// This runs after a console save, so the alternative is replacing a
		// correct registry with built-in defaults because some other file is
		// unreadable — a save of one tool's suites wiping every declaration.
		slog.Error("重建工具注册表失败，保留原来的", logging.Err(err))
		return
	}
	store.Swap(reg)
}

// importableMetadataDir is the metadata directory this deployment configured,
// or empty when it did not configure one.
//
// The difference from ToolMetadataDir matters for exactly one caller: the
// console.yaml import, which moves a file out of the way. Reading a file
// nobody pointed at is harmless; renaming it is not.
//
// ToolMetadataDir falls back to ~/.jelly-agent/tools when nothing is
// configured, so a process pointed at some other database would otherwise
// import the real deployment's file and rename the original. That happened,
// to a developer's own machine, during a test run.
func (e *Engine) importableMetadataDir() string {
	if e.cfg != nil && strings.TrimSpace(e.cfg.Tools.MetadataDir) != "" {
		return e.cfg.Tools.MetadataDir
	}
	if dir := e.configDir(); dir != "" {
		return filepath.Join(dir, "tools")
	}
	return ""
}

// metadataSources is where tool metadata comes from, in layers.
//
// Builtins and the files under the metadata directory declare tools; the
// database declares fields of them and is applied last as a patch (see
// toolreg.Overlay). The order in this slice does not decide that — the
// overlay marker does — but the layering is the thing to see here.
//
// A database that will not open leaves the file layers in place rather than
// failing the registry. The console cannot save without it either way, and a
// deployment that still answers with what its files say is more useful than
// one that answers with nothing.
func (e *Engine) metadataSources() []toolreg.Source {
	sources := e.fileMetadataSources()
	db, err := e.StateDB()
	if err != nil {
		slog.Warn("状态库打不开，控制台声明这一层没有生效", logging.Err(err))
		return sources
	}
	return append(sources, toolreg.NewDBSource(db))
}

// fileMetadataSources is every layer except the database's: the built-ins,
// the bundled declarations and the metadata directory. Separated because the
// watcher already holds the database layer and must not read it again — see
// watchDeclarations.
func (e *Engine) fileMetadataSources() []toolreg.Source {
	sources := []toolreg.Source{jellytool.BuiltinMetadata(), toolreg.BundledMetadata()}
	if dir := e.ToolMetadataDir(); dir != "" {
		sources = append(sources, toolreg.NewFileSource(dir))
	}
	return sources
}

// ToolMetadataDir is where this deployment's tool declarations live.
//
// Resolved rather than read straight off the config, so the default applies
// and every caller — the loader, the console, the writer — agrees on one
// location.
func (e *Engine) ToolMetadataDir() string {
	return config.ToolMetadataDir(e.cfg, e.cfg.SourcePath)
}

// stateReference resolves where the state database is, once.
//
// Every store in it has to agree, and they used to each decide for themselves:
// the metrics recorder and the L2 index called DefaultDBPath directly, so a
// deployment that configured PostgreSQL had its call records and its memory
// index quietly left behind in ~/.jelly-agent/state.db — while sessions,
// events, tool results and task links moved. Every join in the console is
// (session_id, invocation_id), so the halves cannot be in different databases.
func (e *Engine) stateReference() (string, error) {
	if e.stateRef != "" {
		return e.stateRef, nil
	}
	// Beside the config file, the same way the metadata directory is.
	//
	// These two used to disagree: config.ToolMetadataDir derives from where
	// the config came from, while DefaultDBPath was hardcoded at
	// ~/.jelly-agent/state.db. A deployment whose config lived anywhere else
	// therefore had its declarations read from one place and its database in
	// another — and the console.yaml import, which reads one and writes the
	// other, moved a file out of one deployment and put its rows in another.
	// That happened, to a developer's own machine, during a test run.
	if p := e.configDir(); p != "" {
		return filepath.Join(p, "state.db"), nil
	}
	return jellysession.DefaultDBPath()
}

// configDir is the directory the running config came from, or empty.
func (e *Engine) configDir() string {
	if e.cfg == nil || e.cfg.SourcePath == "" || e.cfg.SourcePath == "(env)" {
		return ""
	}
	return filepath.Dir(e.cfg.SourcePath)
}

// StateRef names the state database. Empty means the shared default, which
// the session package resolves for itself.
//
// A reference, not a path: `postgres://…` selects PostgreSQL, anything else is
// a SQLite file. The name says so because the value has not been only a path
// since the dialect seam went in, and a getter called SessionDBPath returning
// a URL is the kind of thing that gets passed to filepath.Dir.
func (e *Engine) StateRef() string { return e.stateRef }

// NewSessionService returns the persistent session store, building it once.
//
// The CLI and web server share one store, so history is consistent across both
// front ends. It is built once because building it is not free: the service
// opens its own connection and runs AutoMigrate, which on PostgreSQL means
// asking the catalogue about four tables over the network. Six request paths
// call this, and each of them was paying for it — 733ms a call, which was most
// of what a page took.
//
// The service is safe for concurrent use; ADK's implementation is a handle on
// a GORM database, not per-request state.
func (e *Engine) NewSessionService() (adksession.Service, error) {
	e.sessionOnce.Do(func() {
		// stateReference, not the raw field. They differ when no DSN is set
		// but the config came from a file: everything else then lives beside
		// that file, while an empty string sends the session store to
		// ~/.jelly-agent/state.db — putting sessions and events in one
		// database and every table that joins against them in another.
		var ref string
		if ref, e.sessionErr = e.stateReference(); e.sessionErr != nil {
			return
		}
		e.sessionSvc, e.sessionClose, e.sessionErr = jellysession.New(ref)
	})
	return e.sessionSvc, e.sessionErr
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

// SetSessionServiceForTest replaces the session store.
//
// Tests need this to observe what the handlers ask of it — how many full
// session loads one page of the task list costs, which is the thing that made
// that page 642ms against PostgreSQL. Nothing in production calls it.
func (e *Engine) SetSessionServiceForTest(svc adksession.Service) {
	e.sessionOnce.Do(func() {}) // so a later NewSessionService does not overwrite this
	e.sessionSvc, e.sessionErr = svc, nil
}
