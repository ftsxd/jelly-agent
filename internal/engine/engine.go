// Package engine assembles jelly-agent's runtime — model, agent, runner,
// session store, and memory layers — from a loaded config. It is the single
// source of truth shared by the CLI (cmd/cli) and the web server (cmd/server),
// so both drive the same agent the same way.
package engine

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	adksession "google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"

	"github.com/jelly-agent/jelly-agent/internal/config"
	jellymcp "github.com/jelly-agent/jelly-agent/internal/mcp"
	"github.com/jelly-agent/jelly-agent/internal/memory"
	jellymodel "github.com/jelly-agent/jelly-agent/internal/model"
	"github.com/jelly-agent/jelly-agent/internal/sandbox"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	"github.com/jelly-agent/jelly-agent/internal/skill"
	jellytool "github.com/jelly-agent/jelly-agent/internal/tool"
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
		log.Printf("[sandbox] backend=%s file=%q args=%v %s dur=%s",
			ev.Backend, ev.File, ev.Args, status, ev.Duration.Round(time.Millisecond))
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

	mcpCtx    context.Context
	mcpCancel context.CancelFunc
	mcpOnce   sync.Once
	mcpSets   map[string]adktool.Toolset // enabled MCP toolsets, keyed by server name
}

// New wraps a loaded config in an engine.
func New(cfg *config.Config) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{cfg: cfg, reg: jellymodel.NewRegistry(cfg), mcpCtx: ctx, mcpCancel: cancel}
}

// Config returns the underlying config.
func (e *Engine) Config() *config.Config { return e.cfg }

// Close releases engine-owned resources. It cancels the MCP context, which
// terminates any stdio MCP subprocesses this engine launched. Safe to call once
// the engine is no longer serving requests (e.g. after a config hot-reload).
func (e *Engine) Close() {
	if e.mcpCancel != nil {
		e.mcpCancel()
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

// BuildAgent constructs the root agent for the named provider (empty = default)
// loading every enabled MCP server. See BuildAgentWith for selective MCP.
func (e *Engine) BuildAgent(provider string) (agent.Agent, config.Provider, *memory.Core, *memory.Search, error) {
	return e.BuildAgentWith(provider, nil)
}

// BuildAgentWith is like BuildAgent but controls which MCP servers are loaded:
// nil mcpNames loads all enabled servers (the default), a non-nil slice loads
// only those named (an empty slice loads none). Returns the resolved provider
// plus the core store and search service so the caller can render memory and
// index turns. search is nil when L2 is disabled.
func (e *Engine) BuildAgentWith(provider string, mcpNames []string) (agent.Agent, config.Provider, *memory.Core, *memory.Search, error) {
	llm, prov, err := e.reg.Get(provider)
	if err != nil {
		return nil, config.Provider{}, nil, nil, err
	}

	core, err := e.Core()
	if err != nil {
		return nil, prov, nil, nil, fmt.Errorf("init memory: %w", err)
	}
	search, err := e.Search()
	if err != nil {
		return nil, prov, nil, nil, err
	}

	tools, err := e.Tools(core, search != nil)
	if err != nil {
		if search != nil {
			search.Close()
		}
		return nil, prov, nil, nil, fmt.Errorf("build tools: %w", err)
	}

	toolsets := e.Toolsets() // nil mcpNames → all enabled MCP servers
	if mcpNames != nil {
		toolsets = e.ToolsetsFor(mcpNames) // selected subset (may be empty)
	}

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

	a, err := llmagent.New(llmagent.Config{
		Name:        "root",
		Model:       llm,
		Description: "jelly-agent root agent with web search and core memory.",
		// InstructionProvider (not Instruction) so MEMORY.md/USER.md (and the
		// skill catalog) are read fresh each turn. Note: ADK then skips {}
		// session-state substitution.
		InstructionProvider: func(agent.ReadonlyContext) (string, error) {
			base := core.Render(RootInstruction)
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
		Tools:    tools,
		Toolsets: toolsets, // external MCP servers (all enabled, or a selected subset)
	})
	if err != nil {
		if search != nil {
			search.Close()
		}
		return nil, prov, nil, nil, fmt.Errorf("create agent: %w", err)
	}
	return a, prov, core, search, nil
}

// NewSessionService opens the persistent SQLite session store at the default
// path. The CLI and web server share one store, so history is consistent across
// both front ends.
func (e *Engine) NewSessionService() (adksession.Service, error) {
	return jellysession.NewSQLite("")
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
