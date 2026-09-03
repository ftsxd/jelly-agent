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

// A tools whitelist can be set from the console, and — the part that matters —
// survives a later save that says nothing about it.
//
// This is the same trap config.Save had: a handler that rebuilds the whole
// record from its input drops every field the input omits, and the symptom (a
// whitelist that vanishes when someone toggles "enabled") points nowhere near
// the cause.
func TestMCPToolsWhitelistRoundTripsAndSurvivesUnrelatedSaves(t *testing.T) {
	s := newEmptyServer(t)

	post := func(body string) {
		t.Helper()
		if w := do(t, s, "POST", "/api/mcp", body); w.Code != http.StatusOK {
			t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
		}
	}
	stored := func() []string {
		t.Helper()
		for _, srv := range s.engine().Config().MCP {
			if srv.Name == "k8s" {
				return srv.Tools
			}
		}
		t.Fatal("server k8s not found in config")
		return nil
	}

	post(`{"name":"k8s","transport":"stdio","command":"x","enabled":true,"tools":["get_pods","get_events"]}`)
	if got := stored(); len(got) != 2 || got[0] != "get_pods" {
		t.Fatalf("whitelist = %v, want it stored", got)
	}
	// It must also come back to the browser, or the console cannot show it.
	if body := do(t, s, "GET", "/api/mcp", "").Body.String(); !strings.Contains(body, "get_pods") {
		t.Errorf("listing does not carry the whitelist: %s", body)
	}

	// A save that says nothing about tools must not wipe it.
	post(`{"name":"k8s","transport":"stdio","command":"x","enabled":false}`)
	if got := stored(); len(got) != 2 {
		t.Errorf("whitelist = %v after an unrelated save; it was wiped", got)
	}

	// An explicit empty array is a real instruction: load everything.
	post(`{"name":"k8s","transport":"stdio","command":"x","enabled":true,"tools":[]}`)
	if got := stored(); len(got) != 0 {
		t.Errorf("whitelist = %v, want an explicit empty array to clear it", got)
	}
}
