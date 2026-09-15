package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

func codeServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := newTestServer(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	s.engine().Config().SourcePath = path
	return s.WithConfigPath(path), path
}

func TestCodeRootsRoundTripAndReportStatus(t *testing.T) {
	s, path := codeServer(t)
	repos := filepath.Join(t.TempDir(), "repos")
	if err := os.MkdirAll(filepath.Join(repos, "project-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repos, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "not-synced-yet")

	body, _ := json.Marshal(codeInput{Roots: []string{repos, missing}})
	if w := do(t, s, "POST", "/api/files", string(body)); w.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", w.Code, w.Body.String())
	}
	raw, err := config.LoadRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Files.Roots) != 2 {
		t.Fatalf("roots not persisted: %+v", raw.Files)
	}

	w := do(t, s, "GET", "/api/files", "")
	var got codeView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Roots) != 2 {
		t.Fatalf("roots = %+v", got.Roots)
	}
	// The point of the page: a path that is fine and a path that is not must be
	// distinguishable at a glance, instead of the tools just going quiet.
	if !got.Roots[0].Exists || len(got.Roots[0].Projects) != 1 || got.Roots[0].Projects[0] != "project-a" {
		t.Errorf("good root reported wrong: %+v", got.Roots[0])
	}
	if got.Roots[1].Exists || got.Roots[1].Error == "" {
		t.Errorf("a missing directory must be reported with a reason: %+v", got.Roots[1])
	}
}

// The one mistake path containment cannot undo: pointing a root AT the agent's
// own state. Everything in there — API keys, the session database — would then
// be legitimately readable, and no amount of symlink checking helps.
func TestCodeRootsRefuseTheAgentsOwnStateDirectory(t *testing.T) {
	s, path := codeServer(t)
	stateDir := filepath.Dir(path)
	nested := filepath.Join(stateDir, "skills")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{stateDir, nested} {
		body, _ := json.Marshal(codeInput{Roots: []string{bad}})
		w := do(t, s, "POST", "/api/files", string(body))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("accepted %q as a code root (status %d)", bad, w.Code)
		}
	}

	// A relative path is refused too: it would resolve against the server's
	// working directory, not the operator's.
	body, _ := json.Marshal(codeInput{Roots: []string{"repos"}})
	if w := do(t, s, "POST", "/api/files", string(body)); w.Code != http.StatusBadRequest {
		t.Fatalf("accepted a relative code root (status %d)", w.Code)
	}
}

// Clearing the list is a real choice — it takes the file tools away again.
func TestCodeRootsCanBeCleared(t *testing.T) {
	s, path := codeServer(t)
	repos := t.TempDir()

	body, _ := json.Marshal(codeInput{Roots: []string{repos}})
	if w := do(t, s, "POST", "/api/files", string(body)); w.Code != http.StatusOK {
		t.Fatalf("save status = %d", w.Code)
	}
	body, _ = json.Marshal(codeInput{Roots: []string{}})
	if w := do(t, s, "POST", "/api/files", string(body)); w.Code != http.StatusOK {
		t.Fatalf("clear status = %d: %s", w.Code, w.Body.String())
	}
	raw, err := config.LoadRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Files.Roots) != 0 {
		t.Fatalf("roots survived a clear: %+v", raw.Files.Roots)
	}
}
