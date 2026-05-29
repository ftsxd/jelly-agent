// Package config loads jelly-agent configuration from a YAML file (with
// ${ENV} expansion) and falls back to environment variables, so the CLI works
// both with a config file and with the Phase 0/1-style env-only setup.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// Provider is one OpenAI-compatible model endpoint.
type Provider struct {
	Name    string `mapstructure:"name" yaml:"name"`
	BaseURL string `mapstructure:"base_url" yaml:"base_url"`
	APIKey  string `mapstructure:"api_key" yaml:"api_key"`
	Model   string `mapstructure:"model" yaml:"model"`
}

// Config is the top-level jelly-agent configuration.
type Config struct {
	DefaultProvider string     `mapstructure:"default_provider" yaml:"default_provider"`
	Providers       []Provider `mapstructure:"providers" yaml:"providers"`

	// SourcePath records where the config came from ("(env)" for the env
	// fallback, "" when nothing was found). Not persisted.
	SourcePath string `mapstructure:"-" yaml:"-"`
}

// Load reads and parses a YAML config file, expanding ${ENV} references in its
// contents before parsing.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expanded := os.ExpandEnv(string(raw))

	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(expanded)); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", path, err)
	}
	c.SourcePath = path
	return &c, nil
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
