// Package config loads jelly-agent configuration from a YAML file (with
// ${ENV} expansion) and falls back to environment variables, so the CLI works
// both with a config file and with the Phase 0/1-style env-only setup.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var envReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Provider is one OpenAI-compatible model endpoint.
type Provider struct {
	Name    string `mapstructure:"name" yaml:"name"`
	BaseURL string `mapstructure:"base_url" yaml:"base_url,omitempty"`
	APIKey  string `mapstructure:"api_key" yaml:"api_key,omitempty"`
	Model   string `mapstructure:"model" yaml:"model,omitempty"`

	// Temperature overrides the endpoint's default sampling temperature. It is a
	// pointer so "unset" is distinguishable from an explicit value.
	Temperature *float64 `mapstructure:"temperature" yaml:"temperature,omitempty"`
	// MaxTokens caps the completion length. Zero ⇒ the endpoint's default.
	MaxTokens int `mapstructure:"max_tokens" yaml:"max_tokens,omitempty"`
	// TimeoutSec bounds how long to wait for the model's response headers (i.e.
	// time to first byte), not the whole exchange — a long stream must not be
	// cut off mid-answer. Zero ⇒ the model layer's default.
	TimeoutSec int `mapstructure:"timeout_sec" yaml:"timeout_sec,omitempty"`
	// MaxRetries bounds automatic retries of transient failures (429 / 5xx /
	// network). Pointer so an explicit 0 (disable retries) is distinguishable
	// from "unset". Nil ⇒ the model layer's default.
	MaxRetries *int `mapstructure:"max_retries" yaml:"max_retries,omitempty"`
}

// History bounds the conversation history sent to the model each turn. Without
// a bound, a few large tool results (e.g. fetch_url pages) push a session past
// the context window; see internal/history.
type History struct {
	// MaxTokens is the history budget. Nil ⇒ the package default; an explicit
	// 0 turns compaction off entirely (the whole history is always sent).
	MaxTokens *int `mapstructure:"max_tokens" yaml:"max_tokens,omitempty"`
	// KeepRecent is how many trailing contents are never dropped, so the
	// current question always survives. Zero ⇒ default.
	KeepRecent int `mapstructure:"keep_recent" yaml:"keep_recent,omitempty"`
	// ToolResultTokens caps an individual tool result once it is selected for
	// shortening. Zero ⇒ default.
	ToolResultTokens int `mapstructure:"tool_result_tokens" yaml:"tool_result_tokens,omitempty"`
}

// Tools configures the tool registry.
type Tools struct {
	// MetadataDir holds YAML files describing tools: the name the model sees,
	// which arguments the host injects, how a result is reduced. One file per
	// backend keeps diffs reviewable. Empty means built-in defaults only.
	MetadataDir string `mapstructure:"metadata_dir" yaml:"metadata_dir,omitempty"`

	// MaxTools caps how many tool schemas reach the model on a turn. Every
	// registered tool costs prompt tokens whether or not it is relevant, and
	// past a few dozen the model also gets worse at choosing from the list.
	//
	// Zero uses the built-in default, which is generous enough to change
	// nothing for a deployment that has not gone looking for this. Set it
	// negative to turn selection off and send every tool, which is what
	// happened before this existed.
	MaxTools int `mapstructure:"max_tools" yaml:"max_tools,omitempty"`
}

// Logging configures the process-wide structured logger. JSON is the default
// because these records are meant to be shipped; text exists for local work.
type Logging struct {
	Level     string `mapstructure:"level" yaml:"level,omitempty"`   // debug|info|warn|error, 空为 info
	Format    string `mapstructure:"format" yaml:"format,omitempty"` // json（默认）|text
	AddSource bool   `mapstructure:"add_source" yaml:"add_source,omitempty"`
}

// Tracing configures OpenTelemetry span export. ADK already instruments the
// agent loop against the global TracerProvider, so this section only decides
// where those spans go — see internal/telemetry.
type Tracing struct {
	Enabled  bool   `mapstructure:"enabled" yaml:"enabled"`
	Endpoint string `mapstructure:"endpoint" yaml:"endpoint,omitempty"` // host:port, no scheme
	Protocol string `mapstructure:"protocol" yaml:"protocol,omitempty"` // grpc (default) | http
	Service  string `mapstructure:"service" yaml:"service,omitempty"`
	// SampleRatio: nil or 1 records every run. Agent runs are low-volume and
	// expensive to reproduce, so sampling them down is rarely what you want.
	SampleRatio *float64 `mapstructure:"sample_ratio" yaml:"sample_ratio,omitempty"`
	// Insecure sends plaintext OTLP; right for localhost or in-cluster.
	Insecure bool `mapstructure:"insecure" yaml:"insecure,omitempty"`
	// CaptureContent puts prompts and model replies into spans. Invaluable
	// while developing, and a data-exposure decision in production — a trace
	// backend rarely has the access controls a database does.
	CaptureContent bool `mapstructure:"capture_content" yaml:"capture_content,omitempty"`
}

// Config is the top-level jelly-agent configuration.
type Config struct {
	DefaultProvider string     `mapstructure:"default_provider" yaml:"default_provider"`
	Providers       []Provider `mapstructure:"providers" yaml:"providers"`
	Memory          Memory     `mapstructure:"memory" yaml:"memory"`
	History         History    `mapstructure:"history" yaml:"history,omitempty"`
	Tracing         Tracing    `mapstructure:"tracing" yaml:"tracing,omitempty"`
	Logging         Logging    `mapstructure:"logging" yaml:"logging,omitempty"`
	Tools           Tools      `mapstructure:"tools" yaml:"tools,omitempty"`
	Skills          Skills     `mapstructure:"skills" yaml:"skills,omitempty"`
	Sandbox         Sandbox    `mapstructure:"sandbox" yaml:"sandbox,omitempty"`
	Web             Web        `mapstructure:"web" yaml:"web,omitempty"`
	// SkillVars holds per-skill variables (skill name → KV), where secret-ish
	// values are masked by the API and may use ${ENV}. Kept here (config, 0600)
	// rather than in the skill files so sharing/exporting a skill omits secrets.
	SkillVars map[string]map[string]string `mapstructure:"skill_vars" yaml:"skill_vars,omitempty"`
	MCP       []MCPServer                  `mapstructure:"mcp" yaml:"mcp,omitempty"`
	Platforms []PlatformBot                `mapstructure:"platforms" yaml:"platforms,omitempty"`
	Schedules []ScheduleTask               `mapstructure:"schedules" yaml:"schedules,omitempty"`

	// DefaultAgent names the agent the CLI/web run when none is specified. Empty
	// ⇒ the built-in single "root" agent (backward compatible).
	DefaultAgent string `mapstructure:"default_agent" yaml:"default_agent,omitempty"`
	// Agents are named agent definitions composable into a coordinator/sub-agent
	// tree (PLAN §multi-agent). Empty ⇒ the engine builds the legacy single agent.
	Agents []AgentDef `mapstructure:"agents" yaml:"agents,omitempty"`

	// SourcePath records where the config came from ("(env)" for the env
	// fallback, "" when nothing was found). Not persisted.
	SourcePath string `mapstructure:"-" yaml:"-"`
}

// MCPServer is one Model Context Protocol server whose tools are merged into the
// agent's tool set. Transport selects how to reach it: "stdio" launches a local
// command and talks over its stdin/stdout; "http" (streamable) and "sse" connect
// to a remote endpoint. Disabled servers are kept in config but not loaded.
type MCPServer struct {
	Name      string            `mapstructure:"name" yaml:"name"`
	Transport string            `mapstructure:"transport" yaml:"transport"`
	Command   string            `mapstructure:"command" yaml:"command,omitempty"`
	Args      []string          `mapstructure:"args" yaml:"args,omitempty"`
	Env       map[string]string `mapstructure:"env" yaml:"env,omitempty"`
	URL       string            `mapstructure:"url" yaml:"url,omitempty"`
	Headers   map[string]string `mapstructure:"headers" yaml:"headers,omitempty"`
	Enabled   bool              `mapstructure:"enabled" yaml:"enabled"`

	// Tools whitelists which of the server's tools to load. Empty loads all.
	//
	// This is the cheapest of the three cuts that keep prompt size independent
	// of registry size, and the only one that happens before a tool exists at
	// all: a Kubernetes MCP server commonly advertises two or three dozen
	// tools, of which a diagnosis needs a handful. The rest cost a schema slot
	// each and add names for the model to confuse.
	Tools []string `mapstructure:"tools" yaml:"tools,omitempty"`
}

// PlatformBot binds an external messaging platform (currently DingTalk) as a
// message entry point: incoming chats are answered by the same engine the web
// console and CLI drive. DingTalk uses Stream mode (an outbound WebSocket), so
// no public callback URL is needed — ClientID/ClientSecret are the bot's
// AppKey/AppSecret. Provider selects which LLM to answer with (empty = default).
type PlatformBot struct {
	Name         string `mapstructure:"name" yaml:"name"`
	Type         string `mapstructure:"type" yaml:"type"` // "dingtalk" | "wechatpadpro"
	Enabled      bool   `mapstructure:"enabled" yaml:"enabled"`
	ClientID     string `mapstructure:"client_id" yaml:"client_id,omitempty"`         // dingtalk AppKey
	ClientSecret string `mapstructure:"client_secret" yaml:"client_secret,omitempty"` // dingtalk AppSecret
	Provider     string `mapstructure:"provider" yaml:"provider,omitempty"`

	// Settings carries platform-specific configuration that doesn't fit the
	// common fields. WeChatPadPro (个人微信) uses it for wechatpad_url,
	// wechatpad_ws, admin_key, token, wxid. Secret-ish keys are masked by the API.
	Settings map[string]string `mapstructure:"settings" yaml:"settings,omitempty"`

	// MCP names the MCP servers this bot's agent loads (a subset of the enabled
	// servers). Empty = no MCP tools for this bot. Lets each bot selectively load
	// MCP instead of always injecting every enabled server.
	MCP []string `mapstructure:"mcp" yaml:"mcp,omitempty"`
}

// AgentDef is a named agent in the multi-agent tree. A coordinator references
// specialists by name in SubAgents; ADK then exposes transfer_to_agent so the
// coordinator's LLM can delegate a turn to the best-matching child (it routes on
// each child's Description). The zero-ish value is valid — empty fields fall
// back to engine defaults (default provider, RootInstruction, all enabled MCP).
type AgentDef struct {
	// Name is the unique identifier ([A-Za-z0-9_-]+), used by chat selection and
	// as the agent's name in the tree.
	Name string `mapstructure:"name" yaml:"name" json:"name"`
	// Description tells a parent coordinator when to delegate here. One line.
	Description string `mapstructure:"description" yaml:"description,omitempty" json:"description,omitempty"`
	// Provider names which configured provider/model this agent runs on. Empty
	// ⇒ the default provider.
	Provider string `mapstructure:"provider" yaml:"provider,omitempty" json:"provider,omitempty"`
	// Instruction is this agent's system instruction. Empty ⇒ RootInstruction.
	Instruction string `mapstructure:"instruction" yaml:"instruction,omitempty" json:"instruction,omitempty"`
	// MCP names the MCP servers this agent loads (a subset of the enabled
	// servers). Empty ⇒ no MCP. Mirrors PlatformBot.MCP semantics.
	MCP []string `mapstructure:"mcp" yaml:"mcp,omitempty" json:"mcp,omitempty"`
	// SubAgents names the child agents this one may transfer to (delegation).
	SubAgents []string `mapstructure:"sub_agents" yaml:"sub_agents,omitempty" json:"sub_agents,omitempty"`
	// Enabled gates whether the agent is selectable / built. Disabled agents are
	// kept in config but skipped.
	Enabled bool `mapstructure:"enabled" yaml:"enabled,omitempty" json:"enabled"`
}

// Skills configures the Agent Skills subsystem (Markdown skill packages loaded
// on demand via the use_skill tool). Dir is where skill files live; empty uses
// the default ~/.jelly-agent/skills.
type Skills struct {
	Dir string `mapstructure:"dir" yaml:"dir,omitempty"`
	// AllowScripts enables the run_script tool (skills can execute bundled
	// scripts). Off by default — it runs code with the user's privileges.
	AllowScripts bool `mapstructure:"allow_scripts" yaml:"allow_scripts,omitempty"`
}

// Web configures access to the local web dashboard. A configured administrator
// is required before either server entry point will start.
type Web struct {
	Admin Admin `mapstructure:"admin" yaml:"admin,omitempty"`
}

// Admin is the one local dashboard administrator. PasswordHash must be a
// bcrypt hash; never store a plaintext password in config.yaml.
type Admin struct {
	Username     string `mapstructure:"username" yaml:"username,omitempty"`
	PasswordHash string `mapstructure:"password_hash" yaml:"password_hash,omitempty"`
	MustChange   bool   `mapstructure:"must_change" yaml:"must_change,omitempty"`
}

// ScheduleTask is a persisted cron-triggered Agent job. Cron uses the standard
// five-field syntax (minute hour day-of-month month day-of-week).
type ScheduleTask struct {
	Name          string `yaml:"name" json:"name"`
	Cron          string `yaml:"cron" json:"cron"`
	Prompt        string `yaml:"prompt" json:"prompt"`
	Agent         string `yaml:"agent,omitempty" json:"agent,omitempty"`
	Provider      string `yaml:"provider,omitempty" json:"provider,omitempty"`
	Skill         string `yaml:"skill,omitempty" json:"skill,omitempty"`
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	RetryCount    int    `yaml:"retry_count,omitempty" json:"retry_count,omitempty"`
	RetryDelaySec int    `yaml:"retry_delay_sec,omitempty" json:"retry_delay_sec,omitempty"`
}

func (a Admin) Configured() bool {
	return strings.TrimSpace(a.Username) != "" && strings.TrimSpace(a.PasswordHash) != ""
}

// Sandbox configures the execution envelope for skill scripts (and future
// run_code). The zero value is valid: the sandbox package applies its defaults
// (60s timeout, 8 KiB output, native best-effort confinement). See PLAN §8
// risk 6.
type Sandbox struct {
	// Backend selects the isolation backend: "" (auto), "native", or "docker".
	// Auto uses native unless AllowDocker is set and a docker binary is found.
	Backend string `mapstructure:"backend" yaml:"backend,omitempty"`
	// AllowDocker permits the auto/explicit docker backend (strong isolation).
	AllowDocker bool `mapstructure:"allow_docker" yaml:"allow_docker,omitempty"`
	// Network allows network access in the docker backend (native cannot
	// restrict it either way). Default false ⇒ docker runs with --network none.
	Network bool `mapstructure:"network" yaml:"network,omitempty"`
	// Image is the docker image used by the docker backend ("" ⇒ python:3-slim).
	Image string `mapstructure:"image" yaml:"image,omitempty"`
	// Resource caps (zero ⇒ sandbox defaults).
	TimeoutSec  int `mapstructure:"timeout_sec" yaml:"timeout_sec,omitempty"`
	MaxOutputKB int `mapstructure:"max_output_kb" yaml:"max_output_kb,omitempty"`
	CPUSeconds  int `mapstructure:"cpu_seconds" yaml:"cpu_seconds,omitempty"`
	MaxProcs    int `mapstructure:"max_procs" yaml:"max_procs,omitempty"`
	MemoryMB    int `mapstructure:"memory_mb" yaml:"memory_mb,omitempty"`
}

// Memory configures the memory subsystem (PLAN §10.5): L1 core memory
// (always on) and L2 session search (opt-in, FTS5-backed).
type Memory struct {
	Core   MemoryCore   `mapstructure:"core" yaml:"core,omitempty"`
	Search MemorySearch `mapstructure:"search" yaml:"search,omitempty"`
}

// MemoryCore configures L1 core memory. Zero values are fine: the memory layer
// substitutes its defaults (~/.jelly-agent/memory, 800/500 token budgets).
type MemoryCore struct {
	Dir                string `mapstructure:"dir" yaml:"dir,omitempty"`
	MemoryBudgetTokens int    `mapstructure:"memory_budget_tokens" yaml:"memory_budget_tokens,omitempty"`
	UserBudgetTokens   int    `mapstructure:"user_budget_tokens" yaml:"user_budget_tokens,omitempty"`
}

// MemorySearch configures L2 session search (PLAN §10.5). Disabled by default;
// when enabled, past-session text is indexed in SQLite FTS5 (sharing state.db)
// and the agent gains a load_memory tool. A zero TopK uses the memory layer's
// default. Backend currently accepts only "fts5"; the vector backend is L3
// (later). Summarize is reserved — model-side compression of results is not
// yet wired.
type MemorySearch struct {
	Enabled   bool   `mapstructure:"enabled" yaml:"enabled,omitempty"`
	Backend   string `mapstructure:"backend" yaml:"backend,omitempty"`
	TopK      int    `mapstructure:"top_k" yaml:"top_k,omitempty"`
	Summarize bool   `mapstructure:"summarize" yaml:"summarize,omitempty"`
}

// Load reads and parses a YAML config file, expanding ${ENV} references in its
// contents before parsing. This is what the runtime uses (keys are real).
func Load(path string) (*Config, error) {
	return load(path, true)
}

// LoadRaw parses a config file WITHOUT ${ENV} expansion, preserving references
// like ${DEEPSEEK_API_KEY} verbatim. It is the basis for editing/saving config
// from the web UI so an unrelated edit never bakes a secret into the file.
func LoadRaw(path string) (*Config, error) {
	return load(path, false)
}

func load(path string, expand bool) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := raw
	if expand {
		content = []byte(expandEnvReferences(string(raw)))
	}

	// yaml.v3 (not viper) so map keys keep their case — viper lowercases them,
	// which would corrupt case-sensitive MCP env var names like GITHUB_TOKEN.
	var c Config
	if err := yaml.Unmarshal(content, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.SourcePath = path
	return &c, nil
}

// expandEnvReferences intentionally recognizes only the documented ${ENV}
// form. os.ExpandEnv would also treat bcrypt's $2a$... password hashes as
// environment variables and silently corrupt them.
func expandEnvReferences(s string) string {
	return envReference.ReplaceAllStringFunc(s, func(ref string) string {
		name := ref[2 : len(ref)-1]
		return os.Getenv(name)
	})
}

// Save writes the config as YAML to path (0600 — it may hold API keys),
// creating parent directories. The memory section is omitted when empty so a
// freshly-created file stays minimal.
func Save(c *Config, path string) error {
	type payload struct {
		DefaultProvider string                       `yaml:"default_provider,omitempty"`
		Providers       []Provider                   `yaml:"providers"`
		Memory          *Memory                      `yaml:"memory,omitempty"`
		History         *History                     `yaml:"history,omitempty"`
		Tracing         *Tracing                     `yaml:"tracing,omitempty"`
		Logging         *Logging                     `yaml:"logging,omitempty"`
		Tools           *Tools                       `yaml:"tools,omitempty"`
		Skills          *Skills                      `yaml:"skills,omitempty"`
		Sandbox         *Sandbox                     `yaml:"sandbox,omitempty"`
		Web             *Web                         `yaml:"web,omitempty"`
		SkillVars       map[string]map[string]string `yaml:"skill_vars,omitempty"`
		MCP             []MCPServer                  `yaml:"mcp,omitempty"`
		Platforms       []PlatformBot                `yaml:"platforms,omitempty"`
		Schedules       []ScheduleTask               `yaml:"schedules,omitempty"`
		DefaultAgent    string                       `yaml:"default_agent,omitempty"`
		Agents          []AgentDef                   `yaml:"agents,omitempty"`
	}
	p := payload{DefaultProvider: c.DefaultProvider, Providers: c.Providers, MCP: c.MCP, Platforms: c.Platforms, Schedules: c.Schedules, SkillVars: c.SkillVars, DefaultAgent: c.DefaultAgent, Agents: c.Agents}
	if c.Web != (Web{}) {
		web := c.Web
		p.Web = &web
	}
	if c.Skills != (Skills{}) {
		sk := c.Skills
		p.Skills = &sk
	}
	if c.Sandbox != (Sandbox{}) {
		sb := c.Sandbox
		p.Sandbox = &sb
	}
	if c.Memory != (Memory{}) {
		m := c.Memory
		p.Memory = &m
	}
	if c.Tracing != (Tracing{}) {
		tr := c.Tracing
		p.Tracing = &tr
	}
	if c.Logging != (Logging{}) {
		lg := c.Logging
		p.Logging = &lg
	}
	if c.Tools != (Tools{}) {
		tl := c.Tools
		p.Tools = &tl
	}
	if c.History != (History{}) {
		h := c.History
		p.History = &h
	}
	out, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

// DefaultUserConfigPath is where the web UI writes config when none exists yet:
// ~/.jelly-agent/config.yaml (always resolvable, no cwd dependency).
func DefaultUserConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".jelly-agent", "config.yaml"), nil
}

// LoadOrEnv resolves a config file (explicit path, $JELLY_CONFIG, then default
// locations) and loads it; if none exists it synthesizes a config from the
// LLM_* environment variables. An empty config (no providers) is returned when
// neither is present, so read-only commands still work.
func LoadOrEnv(explicit string) (*Config, error) {
	if path, ok := resolvePath(explicit); ok {
		c, err := Load(path)
		if err != nil {
			return nil, err
		}
		// A server may create a user config solely to persist its administrator
		// credential. Keep an environment-supplied provider usable in that case.
		if len(c.Providers) == 0 {
			if p, ok := providerFromEnv(); ok {
				c.DefaultProvider = p.Name
				c.Providers = []Provider{p}
			}
		}
		return c, nil
	}
	if p, ok := providerFromEnv(); ok {
		return &Config{DefaultProvider: p.Name, Providers: []Provider{p}, SourcePath: "(env)"}, nil
	}
	return &Config{}, nil
}

// Select returns the named provider, or the default provider when name is "".
func (c *Config) Select(name string) (Provider, error) {
	if name == "" {
		name = c.DefaultProvider
	}
	if name == "" {
		if len(c.Providers) == 1 {
			return c.Providers[0], nil
		}
		return Provider{}, fmt.Errorf("no provider specified and no default_provider set")
	}
	if p, ok := c.provider(name); ok {
		return p, nil
	}
	return Provider{}, fmt.Errorf("provider %q not found", name)
}

func (c *Config) provider(name string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// MaskKey renders an API key for display, hiding all but a short prefix.
func MaskKey(key string) string {
	switch {
	case key == "":
		return "(unset)"
	case len(key) <= 8:
		return "****"
	default:
		return key[:4] + "…" + strings.Repeat("*", 4)
	}
}

// resolvePath finds a config file path, returning false if none exists.
func resolvePath(explicit string) (string, bool) {
	if explicit != "" {
		return explicit, true
	}
	if p := os.Getenv("JELLY_CONFIG"); p != "" {
		return p, true
	}
	candidates := []string{"configs/config.yaml"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".jelly-agent", "config.yaml"))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, true
		}
	}
	return "", false
}

// providerFromEnv builds a provider from the LLM_* environment variables,
// matching the Phase 0/1 CLI behavior.
func providerFromEnv() (Provider, bool) {
	key := os.Getenv("LLM_API_KEY")
	if key == "" {
		return Provider{}, false
	}
	return Provider{
		Name:    "default",
		BaseURL: envOr("LLM_BASE_URL", "https://api.deepseek.com/v1"),
		APIKey:  key,
		Model:   envOr("LLM_MODEL", "deepseek-chat"),
	}, true
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
