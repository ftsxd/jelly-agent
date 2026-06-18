package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSavePreservesEnvRefAcrossRawRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// A config carrying a ${ENV} reference, as LoadRaw would yield it.
	c := &Config{
		DefaultProvider: "deepseek",
		Providers: []Provider{
			{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", APIKey: "${DEEPSEEK_API_KEY}", Model: "deepseek-chat"},
		},
	}
	if err := Save(c, path); err != nil {
		t.Fatalf("save: %v", err)
	}

	// On disk the reference is preserved verbatim.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "${DEEPSEEK_API_KEY}") {
		t.Fatalf("env ref not preserved on disk:\n%s", raw)
	}

	// LoadRaw keeps it literal; Load expands it.
	t.Setenv("DEEPSEEK_API_KEY", "sk-real-secret")
	rawCfg, err := LoadRaw(path)
	if err != nil {
		t.Fatalf("loadraw: %v", err)
	}
	if got := rawCfg.Providers[0].APIKey; got != "${DEEPSEEK_API_KEY}" {
		t.Fatalf("LoadRaw key = %q, want literal ref", got)
	}
	expanded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := expanded.Providers[0].APIKey; got != "sk-real-secret" {
		t.Fatalf("Load key = %q, want expanded", got)
	}
}

func TestSavePermsAndMinimalMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c := &Config{Providers: []Provider{{Name: "x", BaseURL: "u", APIKey: "k", Model: "m"}}}
	if err := Save(c, path); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600 (file holds keys)", info.Mode().Perm())
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "memory:") {
		t.Fatalf("empty memory should be omitted:\n%s", raw)
	}
}
