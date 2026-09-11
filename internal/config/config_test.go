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

func TestLoadPreservesBcryptHashWhileExpandingBracedEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	hash := "$2a$10$abcdefghijklmnopqrstuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuu"
	if err := os.WriteFile(path, []byte("web:\n  admin:\n    username: admin\n    password_hash: "+hash+"\nproviders:\n  - name: x\n    api_key: ${TEST_KEY}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_KEY", "expanded")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Web.Admin.PasswordHash != hash {
		t.Fatalf("bcrypt hash changed to %q", c.Web.Admin.PasswordHash)
	}
	if c.Providers[0].APIKey != "expanded" {
		t.Fatalf("env ref = %q, want expanded", c.Providers[0].APIKey)
	}
}

// Save writes an explicit whitelist of sections, so a field added to Config but
// forgotten there is silently dropped on every web-side save. This round-trips
// a fully-populated config to catch that whole class of bug rather than one
// instance of it.
func TestSaveRoundTripsEverySection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	budget := 12000
	retries := 3
	temp := 0.4
	ratio := 0.5
	in := &Config{
		DefaultProvider: "p1",
		Providers: []Provider{{
			Name: "p1", BaseURL: "http://x", APIKey: "k", Model: "m",
			Temperature: &temp, MaxTokens: 2048, TimeoutSec: 90, MaxRetries: &retries,
		}},
		History: History{MaxTokens: &budget, KeepRecent: 4, ToolResultTokens: 500},
		Logging: Logging{Level: "debug", Format: "text", AddSource: true},
		Tools:   Tools{MetadataDir: "/etc/jelly/tools.d"},
		Tracing: Tracing{
			Enabled: true, Endpoint: "localhost:4317", Protocol: "grpc",
			Service: "jelly", SampleRatio: &ratio, Insecure: true, CaptureContent: true,
		},
		DefaultAgent: "root",
		SkillVars:    map[string]map[string]string{"s": {"K": "V"}},
	}
	in.Memory.Search.Enabled = true
	in.Memory.Search.Backend = "fts5"
	in.Skills.AllowScripts = true
	in.Sandbox.Backend = "native"
	in.Web.Admin = Admin{Username: "admin", PasswordHash: "$2a$hash"}

	if err := Save(in, path); err != nil {
		t.Fatal(err)
	}
	out, err := LoadRaw(path)
	if err != nil {
		t.Fatal(err)
	}

	if out.History.MaxTokens == nil || *out.History.MaxTokens != 12000 ||
		out.History.KeepRecent != 4 || out.History.ToolResultTokens != 500 {
		t.Errorf("history section lost on save: %+v", out.History)
	}
	if len(out.Providers) != 1 {
		t.Fatalf("providers lost: %+v", out.Providers)
	}
	p := out.Providers[0]
	if p.Temperature == nil || *p.Temperature != 0.4 || p.MaxTokens != 2048 ||
		p.TimeoutSec != 90 || p.MaxRetries == nil || *p.MaxRetries != 3 {
		t.Errorf("provider tuning lost on save: %+v", p)
	}
	if !out.Memory.Search.Enabled || out.Memory.Search.Backend != "fts5" {
		t.Errorf("memory section lost: %+v", out.Memory)
	}
	if !out.Skills.AllowScripts {
		t.Error("skills section lost")
	}
	if out.Sandbox.Backend != "native" {
		t.Error("sandbox section lost")
	}
	if out.Web.Admin.Username != "admin" || out.Web.Admin.PasswordHash != "$2a$hash" {
		t.Errorf("web/admin section lost: %+v", out.Web)
	}
	if out.Tools.MetadataDir != "/etc/jelly/tools.d" {
		t.Errorf("tools section lost on save: %+v", out.Tools)
	}
	lg := out.Logging
	if lg.Level != "debug" || lg.Format != "text" || !lg.AddSource {
		t.Errorf("logging section lost on save: %+v", lg)
	}
	tr := out.Tracing
	if !tr.Enabled || tr.Endpoint != "localhost:4317" || tr.Protocol != "grpc" ||
		tr.Service != "jelly" || tr.SampleRatio == nil || *tr.SampleRatio != 0.5 ||
		!tr.Insecure || !tr.CaptureContent {
		t.Errorf("tracing section lost on save: %+v", tr)
	}
	if out.DefaultAgent != "root" || out.SkillVars["s"]["K"] != "V" {
		t.Errorf("default_agent / skill_vars lost: %q %+v", out.DefaultAgent, out.SkillVars)
	}
}

// Saving must not drop storage.dsn.
//
// It did, silently, because Save writes through a payload struct that lists
// every section by hand and this one was missing. An operator points the
// deployment at PostgreSQL, changes anything at all in the console, and the
// next restart is back on the default SQLite file — with every row still in
// the database nobody is reading any more.
func TestSaveKeepsTheStorageDSN(t *testing.T) {
	const dsn = "postgres://jelly:secret@db.example:5432/jelly?sslmode=disable"
	path := filepath.Join(t.TempDir(), "config.yaml")
	in := &Config{
		DefaultProvider: "deepseek",
		Providers:       []Provider{{Name: "deepseek", Model: "deepseek-chat"}},
		Storage:         Storage{DSN: dsn},
	}
	if err := Save(in, path); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Storage.DSN != dsn {
		t.Errorf("storage.dsn = %q after a save/load round trip, want %q", out.Storage.DSN, dsn)
	}
}

// A container cannot reasonably hand-write a storage block into a mounted
// config before its first start, so the DSN can come from the environment.
//
// This is the shape somebody actually hit: compose passed JELLY_STORAGE_DSN,
// the mounted config had no storage section at all, and the container quietly
// used its own SQLite file — a second deployment that could not see the first
// one's declarations, with nothing in the logs to say why.
func TestTheStorageDSNCanComeFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const envDSN = "postgres://env@h:5432/d?sslmode=disable"

	t.Run("文件没说，就用环境里的", func(t *testing.T) {
		write("default_provider: p\nproviders:\n  - name: p\n    base_url: http://x\n    api_key: k\n    model: m\n")
		t.Setenv(StorageDSNEnv, envDSN)
		c, err := LoadOrEnv(path)
		if err != nil {
			t.Fatal(err)
		}
		if c.Storage.DSN != envDSN {
			t.Errorf("dsn = %q，环境变量没起作用", c.Storage.DSN)
		}
	})

	t.Run("文件写了，文件说了算", func(t *testing.T) {
		write("default_provider: p\nproviders:\n  - name: p\n    base_url: http://x\n    api_key: k\n    model: m\nstorage:\n  dsn: postgres://file@h:5432/d\n")
		t.Setenv(StorageDSNEnv, envDSN)
		c, err := LoadOrEnv(path)
		if err != nil {
			t.Fatal(err)
		}
		if c.Storage.DSN != "postgres://file@h:5432/d" {
			t.Errorf("dsn = %q —— 写下来的配置被环境里的变量盖掉了", c.Storage.DSN)
		}
	})

	t.Run("两边都没有，还是配置文件旁边的 SQLite", func(t *testing.T) {
		write("default_provider: p\nproviders:\n  - name: p\n    base_url: http://x\n    api_key: k\n    model: m\n")
		t.Setenv(StorageDSNEnv, "")
		c, err := LoadOrEnv(path)
		if err != nil {
			t.Fatal(err)
		}
		if c.Storage.DSN != "" {
			t.Errorf("dsn = %q，want 空（空才表示用默认的 state.db）", c.Storage.DSN)
		}
	})

	// LoadRaw backs the config editor: folding the environment into it would
	// write the environment into the file the next time somebody saved a
	// provider through the console.
	t.Run("编辑器读的那份不掺环境变量", func(t *testing.T) {
		write("default_provider: p\nproviders:\n  - name: p\n    base_url: http://x\n    api_key: k\n    model: m\n")
		t.Setenv(StorageDSNEnv, envDSN)
		raw, err := LoadRaw(path)
		if err != nil {
			t.Fatal(err)
		}
		if raw.Storage.DSN != "" {
			t.Errorf("LoadRaw 里混进了环境变量的值 %q —— 下次保存就会被写进文件", raw.Storage.DSN)
		}
	})
}
