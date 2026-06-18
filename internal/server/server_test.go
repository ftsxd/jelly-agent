package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
)

// newTestServer builds an API-only server over an engine whose memory lives in a
// temp dir, so tests never touch the real ~/.jelly-agent.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		DefaultProvider: "test",
		Providers:       []config.Provider{{Name: "test", BaseURL: "http://x", APIKey: "sk-secret-key-1234", Model: "m"}},
	}
	cfg.Memory.Core.Dir = t.TempDir()
	return New(engine.New(cfg), nil)
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return m
}

func TestHealth(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "GET", "/api/health", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if m := decode(t, w); m["status"] != "ok" {
		t.Fatalf("status field = %v", m["status"])
	}
}

func TestProvidersMaskKey(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "GET", "/api/providers", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "sk-secret-key-1234") {
		t.Fatalf("raw API key leaked in /api/providers: %s", w.Body.String())
	}
	m := decode(t, w)
	if m["default"] != "test" {
		t.Fatalf("default = %v", m["default"])
	}
}

func TestToolsListsWebSearch(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "GET", "/api/tools", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "web_search") {
		t.Fatalf("web_search missing: %s", w.Body.String())
	}
}

func TestMemoryCore(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "GET", "/api/memory/core", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if _, ok := decode(t, w)["dir"]; !ok {
		t.Fatalf("missing dir field: %s", w.Body.String())
	}
}

func TestMemorySearchDisabledByDefault(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "GET", "/api/memory/search?q=anything", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if decode(t, w)["enabled"] != false {
		t.Fatalf("expected enabled=false when L2 off: %s", w.Body.String())
	}
}

func TestStatsEmpty(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "GET", "/api/stats", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	m := decode(t, w)
	// The session store lives at a shared default path (not the temp dir), so we
	// can't assume it's empty — just assert the field is present and numeric.
	if _, ok := m["sessions"].(float64); !ok {
		t.Fatalf("sessions missing or non-numeric: %v", m["sessions"])
	}
	prov, ok := m["providers"].(map[string]any)
	if !ok || prov["default"] != "test" {
		t.Fatalf("providers.default = %v, want test", m["providers"])
	}
	if mem, ok := m["memory"].(map[string]any); !ok || mem["search_enabled"] != false {
		t.Fatalf("memory.search_enabled = %v, want false", m["memory"])
	}
}

func TestDeleteSessionIdempotent(t *testing.T) {
	s := newTestServer(t)
	// Deleting an unknown session succeeds (idempotent) and returns ok.
	w := do(t, s, "DELETE", "/api/sessions/does-not-exist", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if decode(t, w)["ok"] != true {
		t.Fatalf("ok field = %v", decode(t, w)["ok"])
	}
}

func TestChatRejectsEmptyMessage(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "POST", "/api/chat/stream", `{"message":"  "}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestChatRejectsUnknownField(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "POST", "/api/chat/stream", `{"message":"hi","bogus":1}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown field", w.Code)
	}
}

func TestSPAFallback(t *testing.T) {
	index := []byte("<!doctype html><title>spa</title>")
	fsys := fstest.MapFS{
		"index.html":           {Data: index},
		"assets/app-abc123.js": {Data: []byte("console.log(1)")},
	}
	s := New(engine.New(&config.Config{}), fsys)

	// Real asset is served as-is with an immutable cache header.
	w := do(t, s, "GET", "/assets/app-abc123.js", "")
	if w.Code != http.StatusOK {
		t.Fatalf("asset status = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset cache-control = %q", cc)
	}

	// Unknown deep link falls back to index.html (client-side routing).
	w = do(t, s, "GET", "/sessions", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "spa") {
		t.Fatalf("deep-link fallback failed: code=%d body=%q", w.Code, w.Body.String())
	}

	// API routes are not shadowed by the SPA handler.
	w = do(t, s, "GET", "/api/health", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("api route shadowed by SPA: code=%d", w.Code)
	}
}
