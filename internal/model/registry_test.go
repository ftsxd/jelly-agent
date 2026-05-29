package model

import (
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

func TestRegistryGetDefault(t *testing.T) {
	reg := NewRegistry(&config.Config{
		DefaultProvider: "ds",
		Providers: []config.Provider{
			{Name: "ds", BaseURL: "https://api.deepseek.com/v1", APIKey: "k", Model: "deepseek-chat"},
		},
	})
	llm, prov, err := reg.Get("")
	if err != nil {
		t.Fatalf("Get(\"\"): %v", err)
	}
	if prov.Name != "ds" || llm.Name() != "deepseek-chat" {
		t.Errorf("got provider %q model %q, want ds/deepseek-chat", prov.Name, llm.Name())
	}
}

func TestRegistryEmptyModel(t *testing.T) {
	reg := NewRegistry(&config.Config{
		DefaultProvider: "x",
		Providers:       []config.Provider{{Name: "x", APIKey: "k"}}, // no Model
	})
	if _, _, err := reg.Get(""); err == nil {
		t.Fatal("expected error for provider with empty model")
	}
}

func TestRegistryUnknownProvider(t *testing.T) {
	reg := NewRegistry(&config.Config{
		Providers: []config.Provider{{Name: "x", Model: "m"}},
	})
	if _, _, err := reg.Get("nope"); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
