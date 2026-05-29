package model

import (
	"fmt"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

// Registry builds model.LLM instances from configured providers.
//
// It intentionally does NOT model per-provider capabilities (reasoning,
// stream-usage, tool-schema mode, ...): we currently target a single class of
// OpenAI-compatible endpoints and handle divergences ad hoc. A capability
// layer can be introduced when a second provider class (e.g. native Gemini)
// is added.
type Registry struct {
	cfg *config.Config
}

// NewRegistry wraps a loaded config.
func NewRegistry(cfg *config.Config) *Registry {
	return &Registry{cfg: cfg}
}

// Get builds the LLM for the named provider (or the default when name is "")
// and returns the resolved provider for display.
func (r *Registry) Get(name string) (*OpenAILLM, config.Provider, error) {
	p, err := r.cfg.Select(name)
	if err != nil {
		return nil, config.Provider{}, err
	}
	if p.Model == "" {
		return nil, p, fmt.Errorf("provider %q has no model configured", p.Name)
	}
	llm := New(ProviderConfig{
		Name:    p.Name,
		BaseURL: p.BaseURL,
		APIKey:  p.APIKey,
		Model:   p.Model,
	})
	return llm, p, nil
}
