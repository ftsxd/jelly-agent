package server

import (
	"net/http"
	"strings"
	"testing"

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

func TestProviderCreateRequiresFields(t *testing.T) {
	s := newEmptyServer(t)
	w := do(t, s, "POST", "/api/providers", `{"name":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (missing base_url/model)", w.Code)
	}
}
