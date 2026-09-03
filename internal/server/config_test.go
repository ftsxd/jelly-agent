package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
)

// newEmptyServer starts from no config, with HOME redirected to a temp dir so
// writes land in <tmp>/.jelly-agent/config.yaml and never touch the real home.
func newEmptyServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return New(engine.New(&config.Config{}), nil)
}

func TestProviderCreateUpdateDelete(t *testing.T) {
	s := newEmptyServer(t)

	// Create — becomes default automatically as the first provider.
	w := do(t, s, "POST", "/api/providers",
		`{"name":"deepseek","base_url":"https://api.deepseek.com/v1","api_key":"sk-secret-123456","model":"deepseek-chat","make_default":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}

	// GET reflects the new provider, default flag set, key masked.
	w = do(t, s, "GET", "/api/providers", "")
	body := w.Body.String()
	if strings.Contains(body, "sk-secret-123456") {
		t.Fatalf("raw key leaked: %s", body)
	}
	m := decode(t, w)
	if m["default"] != "deepseek" {
		t.Fatalf("default = %v", m["default"])
	}

	// Update with empty key keeps the stored key; model changes; engine reloads.
	w = do(t, s, "POST", "/api/providers",
		`{"name":"deepseek","base_url":"https://api.deepseek.com/v1","api_key":"","model":"deepseek-reasoner"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", w.Code, w.Body.String())
	}
	if got := s.engine().Config().Providers[0]; got.APIKey != "sk-secret-123456" || got.Model != "deepseek-reasoner" {
		t.Fatalf("update lost key or model: %+v", got)
	}

	// Delete clears it and the default.
	w = do(t, s, "DELETE", "/api/providers/deepseek", "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", w.Code, w.Body.String())
	}
	if got := len(s.engine().Config().Providers); got != 0 {
		t.Fatalf("providers after delete = %d, want 0", got)
	}
}

// TestConfigFileHotReload verifies Watch picks up an edit made directly to the
// config file (not via the web UI) and swaps the running engine.
func TestConfigFileHotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path,
		[]byte("default_provider: a\nproviders:\n  - {name: a, base_url: http://x, model: m}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(engine.New(cfg), nil).WithConfigPath(path)
	s.pollInterval = 5 * time.Millisecond
	// The watcher logs each reload; silence it so the test output stays about
	// the test. Logging is process-wide now, so this is a logger swap rather
	// than a per-server sink.
	quietLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Watch(ctx)
	// Let the watcher capture its baseline signature before we edit, so the edit
	// registers as a change rather than being folded into the initial read.
	time.Sleep(50 * time.Millisecond)

	// External edit: switch the default provider to b (and grow the file so the
	// change is unmistakable even on a coarse-modtime filesystem).
	if err := os.WriteFile(path,
		[]byte("default_provider: b\nproviders:\n  - {name: a, base_url: http://x, model: m}\n  - {name: b, base_url: http://y, model: m}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for s.engine().Config().DefaultProvider != "b" {
		if time.Now().After(deadline) {
			t.Fatalf("hot reload did not apply; default = %q", s.engine().Config().DefaultProvider)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMemorySearchToggle drives the L2 enable/disable endpoint end to end:
// persist to config + hot-reload + reflect in the engine and /api/memory/core.
func TestMemorySearchToggle(t *testing.T) {
	s := newEmptyServer(t)

	if s.engine().SearchEnabled() {
		t.Fatal("expected L2 search off initially")
	}

	w := do(t, s, "PUT", "/api/memory/search", `{"enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("enable status = %d: %s", w.Code, w.Body.String())
	}
	if !s.engine().SearchEnabled() {
		t.Fatal("engine did not pick up enabled=true after reload")
	}
	if m := decode(t, w); m["enabled"] != true {
		t.Fatalf("response enabled = %v, want true", m["enabled"])
	}

	// /api/memory/core surfaces the new state for the UI's initial render.
	w = do(t, s, "GET", "/api/memory/core", "")
	if decode(t, w)["search_enabled"] != true {
		t.Fatalf("memory/core search_enabled not true: %s", w.Body.String())
	}

	// Disabling again flips it back.
	w = do(t, s, "PUT", "/api/memory/search", `{"enabled":false}`)
	if w.Code != http.StatusOK || s.engine().SearchEnabled() {
		t.Fatalf("disable failed: code=%d enabled=%v", w.Code, s.engine().SearchEnabled())
	}
}

func TestProviderCreateRequiresFields(t *testing.T) {
	s := newEmptyServer(t)
	w := do(t, s, "POST", "/api/providers", `{"name":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (missing base_url/model)", w.Code)
	}
}

// quietLogs points the default logger at io.Discard for the duration of a test.
func quietLogs(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}
