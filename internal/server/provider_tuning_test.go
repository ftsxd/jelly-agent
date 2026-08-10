package server

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/history"
)

// newProviderServer builds a server whose config lives in a temp file, so
// POST /api/providers actually writes and reloads.
func newProviderServer(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		DefaultProvider: "test",
		Providers:       []config.Provider{{Name: "test", BaseURL: "http://x", APIKey: "sk-secret", Model: "m"}},
		SourcePath:      path,
	}
	cfg.Memory.Core.Dir = t.TempDir()
	if err := config.Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	return New(engine.New(cfg), nil).WithConfigPath(path), path
}

// reloadProvider reads the named provider back off disk.
func reloadProvider(t *testing.T, path, name string) config.Provider {
	t.Helper()
	raw, err := config.LoadRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := indexOfProvider(raw.Providers, name)
	if idx < 0 {
		t.Fatalf("provider %q missing from %s", name, path)
	}
	return raw.Providers[idx]
}

func TestSaveProviderPersistsTuningFields(t *testing.T) {
	s, path := newProviderServer(t)

	body := `{"name":"test","base_url":"http://x","model":"m",
		"temperature":0.3,"max_tokens":2048,"timeout_sec":90,"max_retries":5}`
	if w := do(t, s, "POST", "/api/providers", body); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}

	p := reloadProvider(t, path, "test")
	if p.Temperature == nil || *p.Temperature != 0.3 {
		t.Errorf("temperature = %v, want 0.3", p.Temperature)
	}
	if p.MaxTokens != 2048 {
		t.Errorf("max_tokens = %d, want 2048", p.MaxTokens)
	}
	if p.TimeoutSec != 90 {
		t.Errorf("timeout_sec = %d, want 90", p.TimeoutSec)
	}
	if p.MaxRetries == nil || *p.MaxRetries != 5 {
		t.Errorf("max_retries = %v, want 5", p.MaxRetries)
	}
}

// The list view's "set as default" button posts only name/base_url/model. That
// partial update must not wipe tuning that was configured earlier.
func TestSaveProviderPartialUpdateKeepsTuning(t *testing.T) {
	s, path := newProviderServer(t)

	full := `{"name":"test","base_url":"http://x","model":"m",
		"temperature":0.3,"max_tokens":2048,"timeout_sec":90,"max_retries":5}`
	if w := do(t, s, "POST", "/api/providers", full); w.Code != 200 {
		t.Fatalf("seed: %d %s", w.Code, w.Body)
	}

	partial := `{"name":"test","base_url":"http://x","model":"m","api_key":"","make_default":true}`
	if w := do(t, s, "POST", "/api/providers", partial); w.Code != 200 {
		t.Fatalf("partial: %d %s", w.Code, w.Body)
	}

	p := reloadProvider(t, path, "test")
	if p.Temperature == nil || *p.Temperature != 0.3 {
		t.Errorf("temperature wiped: %v", p.Temperature)
	}
	if p.MaxTokens != 2048 {
		t.Errorf("max_tokens wiped: %d", p.MaxTokens)
	}
	if p.TimeoutSec != 90 {
		t.Errorf("timeout_sec wiped: %d", p.TimeoutSec)
	}
	if p.MaxRetries == nil || *p.MaxRetries != 5 {
		t.Errorf("max_retries wiped: %v", p.MaxRetries)
	}
	// The partial update must still do its actual job.
	if p.APIKey != "sk-secret" {
		t.Errorf("api key changed: %q", p.APIKey)
	}
}

// An explicit zero clears a field back to the endpoint default; max_retries is
// the exception, where 0 means "never retry" and must be stored as such.
func TestSaveProviderZeroClearsTuning(t *testing.T) {
	s, path := newProviderServer(t)

	full := `{"name":"test","base_url":"http://x","model":"m",
		"temperature":0.3,"max_tokens":2048,"timeout_sec":90,"max_retries":5}`
	if w := do(t, s, "POST", "/api/providers", full); w.Code != 200 {
		t.Fatalf("seed: %d %s", w.Code, w.Body)
	}

	zeroed := `{"name":"test","base_url":"http://x","model":"m",
		"temperature":0,"max_tokens":0,"timeout_sec":0,"max_retries":0}`
	if w := do(t, s, "POST", "/api/providers", zeroed); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}

	p := reloadProvider(t, path, "test")
	if p.Temperature != nil {
		t.Errorf("temperature = %v, want nil (0 is untransmittable, so unset)", *p.Temperature)
	}
	if p.MaxTokens != 0 || p.TimeoutSec != 0 {
		t.Errorf("max_tokens/timeout_sec = %d/%d, want 0/0", p.MaxTokens, p.TimeoutSec)
	}
	if p.MaxRetries == nil || *p.MaxRetries != 0 {
		t.Errorf("max_retries = %v, want explicit 0 (retries disabled)", p.MaxRetries)
	}
}

func TestSaveProviderClampsNegativeTuning(t *testing.T) {
	s, path := newProviderServer(t)

	body := `{"name":"test","base_url":"http://x","model":"m",
		"temperature":-1,"max_tokens":-5,"timeout_sec":-30,"max_retries":-2}`
	if w := do(t, s, "POST", "/api/providers", body); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}

	p := reloadProvider(t, path, "test")
	if p.Temperature != nil {
		t.Errorf("negative temperature stored: %v", *p.Temperature)
	}
	if p.MaxTokens != 0 || p.TimeoutSec != 0 {
		t.Errorf("negative values stored: %d/%d", p.MaxTokens, p.TimeoutSec)
	}
	if p.MaxRetries == nil || *p.MaxRetries != 0 {
		t.Errorf("max_retries = %v, want clamped 0", p.MaxRetries)
	}
}

// The list endpoint must echo the tuning back so the edit form can prefill and
// the "set as default" round-trip stays lossless.
func TestListProvidersReturnsTuning(t *testing.T) {
	s, _ := newProviderServer(t)

	body := `{"name":"test","base_url":"http://x","model":"m",
		"temperature":0.5,"max_tokens":1024,"timeout_sec":45,"max_retries":3}`
	if w := do(t, s, "POST", "/api/providers", body); w.Code != 200 {
		t.Fatalf("seed: %d %s", w.Code, w.Body)
	}

	w := do(t, s, "GET", "/api/providers", "")
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var got struct {
		Providers []struct {
			Name        string   `json:"name"`
			Temperature *float64 `json:"temperature"`
			MaxTokens   int      `json:"max_tokens"`
			TimeoutSec  int      `json:"timeout_sec"`
			MaxRetries  *int     `json:"max_retries"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("got %d providers, want 1", len(got.Providers))
	}
	p := got.Providers[0]
	if p.Temperature == nil || *p.Temperature != 0.5 || p.MaxTokens != 1024 ||
		p.TimeoutSec != 45 || p.MaxRetries == nil || *p.MaxRetries != 3 {
		t.Errorf("tuning not echoed: %+v", p)
	}
}

// The fetch_url bench endpoint must keep the tool's own guards: a loopback or
// private target is refused here exactly as it would be for the agent.
func TestToolFetchRejectsNonPublicTarget(t *testing.T) {
	s, _ := newProviderServer(t)
	w := do(t, s, "POST", "/api/tools/fetch", `{"url":"http://127.0.0.1:9/","max_chars":100}`)
	if w.Code != 400 {
		t.Errorf("loopback fetch = %d, want 400: %s", w.Code, w.Body)
	}
}

func TestToolFetchRejectsBadScheme(t *testing.T) {
	s, _ := newProviderServer(t)
	w := do(t, s, "POST", "/api/tools/fetch", `{"url":"file:///etc/passwd"}`)
	if w.Code != 400 {
		t.Errorf("file:// fetch = %d, want 400: %s", w.Code, w.Body)
	}
}

// reloadHistory reads the history section back off disk.
func reloadHistory(t *testing.T, path string) config.History {
	t.Helper()
	raw, err := config.LoadRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw.History
}

func TestSetHistoryPersistsAndClamps(t *testing.T) {
	s, path := newProviderServer(t)

	if w := do(t, s, "PUT", "/api/history",
		`{"max_tokens":12000,"keep_recent":4,"tool_result_tokens":500}`); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	h := reloadHistory(t, path)
	if h.MaxTokens == nil || *h.MaxTokens != 12000 {
		t.Errorf("max_tokens = %v, want 12000", h.MaxTokens)
	}
	if h.KeepRecent != 4 || h.ToolResultTokens != 500 {
		t.Errorf("keep_recent/tool_result_tokens = %d/%d, want 4/500", h.KeepRecent, h.ToolResultTokens)
	}

	// Negatives are clamped rather than rejected.
	if w := do(t, s, "PUT", "/api/history",
		`{"max_tokens":-5,"keep_recent":-1,"tool_result_tokens":-9}`); w.Code != 200 {
		t.Fatalf("clamp save: %d %s", w.Code, w.Body)
	}
	h = reloadHistory(t, path)
	if h.MaxTokens == nil || *h.MaxTokens != 0 || h.KeepRecent != 0 || h.ToolResultTokens != 0 {
		t.Errorf("negatives not clamped: %+v", h)
	}
}

// null means "use the package default" while an explicit 0 disables compaction;
// the two must not collapse into the same stored state.
func TestSetHistoryNullDiffersFromZero(t *testing.T) {
	s, path := newProviderServer(t)

	if w := do(t, s, "PUT", "/api/history", `{"max_tokens":0,"keep_recent":2}`); w.Code != 200 {
		t.Fatalf("zero: %d %s", w.Code, w.Body)
	}
	if h := reloadHistory(t, path); h.MaxTokens == nil || *h.MaxTokens != 0 {
		t.Errorf("explicit 0 (compaction off) not stored: %v", h.MaxTokens)
	}

	if w := do(t, s, "PUT", "/api/history", `{"max_tokens":null,"keep_recent":2}`); w.Code != 200 {
		t.Fatalf("null: %d %s", w.Code, w.Body)
	}
	if h := reloadHistory(t, path); h.MaxTokens != nil {
		t.Errorf("null should clear back to the default, got %d", *h.MaxTokens)
	}
}

func TestGetHistoryReportsDefaults(t *testing.T) {
	s, _ := newProviderServer(t)
	w := do(t, s, "GET", "/api/history", "")
	if w.Code != 200 {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	var got struct {
		MaxTokens *int           `json:"max_tokens"`
		Defaults  map[string]int `json:"defaults"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxTokens != nil {
		t.Errorf("unset max_tokens should be null, got %d", *got.MaxTokens)
	}
	if got.Defaults["max_tokens"] != history.DefaultMaxTokens {
		t.Errorf("defaults.max_tokens = %d, want %d", got.Defaults["max_tokens"], history.DefaultMaxTokens)
	}
}
