// Package config loads jelly-agent configuration from a YAML file (with
// ${ENV} expansion) and falls back to environment variables, so the CLI works
// both with a config file and with the Phase 0/1-style env-only setup.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider is one OpenAI-compatible model endpoint.
type Provider struct {
	Name    string `mapstructure:"name" yaml:"name"`
	BaseURL string `mapstructure:"base_url" yaml:"base_url,omitempty"`
	APIKey  string `mapstructure:"api_key" yaml:"api_key,omitempty"`
	Model   string `mapstructure:"model" yaml:"model,omitempty"`
}

// Config is the top-level jelly-agent configuration.
type Config struct {
	DefaultProvider string        `mapstructure:"default_provider" yaml:"default_provider"`
	Providers       []Provider    `mapstructure:"providers" yaml:"providers"`
	Memory          Memory        `mapstructure:"memory" yaml:"memory"`
	MCP             []MCPServer    `mapstructure:"mcp" yaml:"mcp,omitempty"`
	Platforms       []PlatformBot `mapstructure:"platforms" yaml:"platforms,omitempty"`

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
		content = []byte(os.ExpandEnv(string(raw)))
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

// Save writes the config as YAML to path (0600 — it may hold API keys),
// creating parent directories. The memory section is omitted when empty so a
// freshly-created file stays minimal.
func Save(c *Config, path string) error {
	type payload struct {
		DefaultProvider string        `yaml:"default_provider,omitempty"`
		Providers       []Provider    `yaml:"providers"`
		Memory          *Memory       `yaml:"memory,omitempty"`
		MCP             []MCPServer    `yaml:"mcp,omitempty"`
		Platforms       []PlatformBot `yaml:"platforms,omitempty"`
	}
	p := payload{DefaultProvider: c.DefaultProvider, Providers: c.Providers, MCP: c.MCP, Platforms: c.Platforms}
	if c.Memory != (Memory{}) {
		m := c.Memory
		p.Memory = &m
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
		return Load(path)
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
