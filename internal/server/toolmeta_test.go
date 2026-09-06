package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

type declBody struct {
	Dir   string        `json:"dir"`
	File  string        `json:"file"`
	Tools []toolDeclDTO `json:"tools"`
	Kinds []struct {
		Value string `json:"value"`
		Label string `json:"label"`
	} `json:"kinds"`
}

// The layer existed and nobody could reach it: declaring what an MCP tool
// produces meant writing a YAML file AND pointing a config key at it, and the
// key had no default. This is the round trip that removes both.
func TestDeclaringAToolFromTheConsole(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	// Nothing declared yet, and the page still knows where things go and what
	// the choices are.
	w := do(t, s, "GET", "/api/tools/metadata", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var before declBody
	if err := json.Unmarshal(w.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if before.Dir != dir || before.File != consoleFile {
		t.Errorf("dir = %q file = %q", before.Dir, before.File)
	}
	if len(before.Kinds) == 0 {
		t.Error("the console has no vocabulary to offer")
	}
	for _, d := range before.Tools {
		if d.Name == "query_instant" {
			t.Fatal("precondition: query_instant should not be declared yet")
		}
	}

	body := `{"name":"query_instant","server":"n9e-mcp","produces":"metric_series","side_effect":"read_only"}`
	if w := do(t, s, "POST", "/api/tools/metadata", body); w.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", w.Code, w.Body.String())
	}

	// It is on disk, in the file the console owns.
	raw, err := os.ReadFile(filepath.Join(dir, consoleFile))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(raw), "query_instant") || !contains(string(raw), "metric_series") {
		t.Errorf("file = %s", raw)
	}

	// And the registry has it, which is the only thing that makes it real.
	w = do(t, s, "GET", "/api/tools/metadata", "")
	var after declBody
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	var got *toolDeclDTO
	for i := range after.Tools {
		if after.Tools[i].Name == "query_instant" {
			got = &after.Tools[i]
		}
	}
	if got == nil {
		t.Fatalf("the declaration did not reach the registry: %+v", after.Tools)
	}
	if got.Produces != "metric_series" || got.Source != "console" {
		t.Errorf("declaration = %+v", got)
	}
}

// Clearing a declaration removes it rather than storing a blank one: "not
// declared" and "declared as nothing" would look the same downstream, and only
// the first is true.
func TestClearingADeclarationRemovesIt(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	do(t, s, "POST", "/api/tools/metadata", `{"name":"query_range","server":"n9e-mcp","produces":"metric_series"}`)
	do(t, s, "POST", "/api/tools/metadata", `{"name":"query_range","server":"n9e-mcp","produces":""}`)

	raw, _ := os.ReadFile(filepath.Join(dir, consoleFile))
	if contains(string(raw), "query_range") {
		t.Errorf("the cleared declaration is still on disk: %s", raw)
	}
}

// A value the domain model does not know must be refused at the edge, not
// written to a file that then fails to load for everything else in it.
func TestUnknownValuesAreRefused(t *testing.T) {
	s := newTestServer(t)
	s.engine().Config().Tools.MetadataDir = t.TempDir()
	for _, body := range []string{
		`{"name":"x","produces":"not_a_kind"}`,
		`{"name":"x","side_effect":"whatever"}`,
		`{"name":"","produces":"config"}`,
	} {
		if w := do(t, s, "POST", "/api/tools/metadata", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s → %d, want 400", body, w.Code)
		}
	}
}

// The directory has a default, because a capability reachable only through a
// path nobody knows about is one nobody uses — which is exactly what happened.
func TestTheMetadataDirectoryHasADefault(t *testing.T) {
	cfg := &config.Config{}
	if got := config.ToolMetadataDir(cfg, "/etc/jelly/config.yaml"); got != "/etc/jelly/tools" {
		t.Errorf("beside the config: %q", got)
	}
	// An explicit setting still wins.
	cfg.Tools.MetadataDir = "/custom/place"
	if got := config.ToolMetadataDir(cfg, "/etc/jelly/config.yaml"); got != "/custom/place" {
		t.Errorf("explicit setting ignored: %q", got)
	}
	// Env-only config still resolves somewhere rather than to nothing.
	if got := config.ToolMetadataDir(&config.Config{}, "(env)"); got == "" {
		t.Error("no directory at all for an env-only deployment")
	}
}

// A console declaration must not set a backend.
//
// Backend filters Registry.Available, and a view that asks without an incident
// would then see none of the declared tools — which is the bug that made the
// task view report "执行工具" for tools that were, in fact, declared.
func TestAConsoleDeclarationDoesNotSetABackend(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	do(t, s, "POST", "/api/tools/metadata", `{"name":"list_targets","server":"n9e-mcp","produces":"workload_status"}`)

	reg := s.engine().ToolRegistry()
	m, ok := reg.Lookup("list_targets")
	if !ok || m.Produces != "workload_status" {
		t.Fatalf("lookup = %+v ok=%v", m, ok)
	}
	if m.Backend != "" {
		t.Errorf("backend = %q", m.Backend)
	}

	// The property that matters, and the one whose absence caused the bug:
	// Available filters by backend, so a declaration carrying one is invisible
	// to every view that asks without an incident — which is every view that
	// needed it.
	seen := false
	for _, a := range reg.Available(nil) {
		if a.Name == "list_targets" {
			seen = true
		}
	}
	if !seen {
		t.Error("the declared tool is invisible to Available(nil), so no incident-less view can use it")
	}
}

// A declaration must reach the policy, not only the display.
//
// The gateway is built once per engine and used to hold a frozen snapshot of
// the registry, so declaring a tool from the console changed what the task
// view said about it and not what the gateway would allow — two answers to one
// question, and the quieter one was the one that mattered.
func TestADeclarationReachesTheGatewayNotJustTheDisplay(t *testing.T) {
	s := newTestServer(t)
	s.engine().Config().Tools.MetadataDir = t.TempDir()

	gw := s.engine().ToolGateway()
	if _, ok := gw.Metadata("restart_service"); ok {
		t.Fatal("precondition: restart_service should be unknown")
	}

	body := `{"name":"restart_service","server":"k8s-mcp","produces":"text","side_effect":"mutating_risky"}`
	if w := do(t, s, "POST", "/api/tools/metadata", body); w.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", w.Code, w.Body.String())
	}

	// Same gateway instance — no engine rebuild, because rebuilding would kill
	// every MCP subprocess for the sake of one dropdown.
	m, ok := gw.Metadata("restart_service")
	if !ok {
		t.Fatal("the gateway still cannot see the declaration")
	}
	if m.SideEffect != "mutating_risky" {
		t.Errorf("side effect = %q; the policy would treat it as something else", m.SideEffect)
	}
}
