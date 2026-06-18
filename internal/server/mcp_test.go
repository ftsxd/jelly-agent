package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestMCPCreateListSecretsHidden(t *testing.T) {
	s := newEmptyServer(t)

	w := do(t, s, "POST", "/api/mcp",
		`{"name":"fs","transport":"stdio","command":"npx","args":["-y","srv"],"env":{"GITHUB_TOKEN":"ghp_secret"},"enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}

	w = do(t, s, "GET", "/api/mcp", "")
	body := w.Body.String()
	if strings.Contains(body, "ghp_secret") {
		t.Fatalf("secret env value leaked to listing: %s", body)
	}
	// Key case must survive (yaml.v3, not viper which lowercases).
	if !strings.Contains(body, "GITHUB_TOKEN") {
		t.Fatalf("env key missing or case-mangled: %s", body)
	}
}

func TestMCPUpdateKeepsSecretOnEmptyValue(t *testing.T) {
	s := newEmptyServer(t)
	do(t, s, "POST", "/api/mcp",
		`{"name":"fs","transport":"stdio","command":"npx","env":{"GITHUB_TOKEN":"ghp_secret"},"enabled":true}`)

	// Edit the command, resubmitting the env key with an empty value.
	w := do(t, s, "POST", "/api/mcp",
		`{"name":"fs","transport":"stdio","command":"npx-2","env":{"GITHUB_TOKEN":""},"enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", w.Code, w.Body.String())
	}
	got := s.engine().Config().MCP[0]
	if got.Command != "npx-2" {
		t.Fatalf("command not updated: %q", got.Command)
	}
	if got.Env["GITHUB_TOKEN"] != "ghp_secret" {
		t.Fatalf("secret dropped on empty-value update: %q", got.Env["GITHUB_TOKEN"])
	}
}

func TestMCPValidationAndDelete(t *testing.T) {
	s := newEmptyServer(t)

	if w := do(t, s, "POST", "/api/mcp", `{"name":"x","transport":"stdio"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("stdio without command: status = %d, want 400", w.Code)
	}
	if w := do(t, s, "POST", "/api/mcp", `{"name":"y","transport":"http"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("http without url: status = %d, want 400", w.Code)
	}

	do(t, s, "POST", "/api/mcp", `{"name":"fs","transport":"stdio","command":"npx","enabled":true}`)
	if w := do(t, s, "DELETE", "/api/mcp/fs", ""); w.Code != http.StatusOK {
		t.Fatalf("delete status = %d", w.Code)
	}
	if n := len(s.engine().Config().MCP); n != 0 {
		t.Fatalf("MCP servers after delete = %d, want 0", n)
	}
}
