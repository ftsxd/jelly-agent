package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

func builtinNames(t *testing.T, e *Engine) map[string]bool {
	t.Helper()
	tools, err := e.Tools(nil, false)
	if err != nil {
		t.Fatalf("build tools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name()] = true
	}
	return names
}

// Filesystem access is opt-in: an agent gets read_file/list_dir/grep_files only
// because an operator named a directory. Registering them unconditionally would
// hand every diagnosis agent the disk it has no business reading.
func TestFileToolsAppearOnlyWhenRootsAreConfigured(t *testing.T) {
	bare := New(&config.Config{})
	for _, name := range []string{"read_file", "list_dir", "grep_files"} {
		if builtinNames(t, bare)[name] {
			t.Errorf("%s was registered with no files.roots configured", name)
		}
	}

	repos := filepath.Join(t.TempDir(), "repos")
	if err := os.MkdirAll(repos, 0o755); err != nil {
		t.Fatal(err)
	}
	withRoots := New(&config.Config{Files: config.Files{Roots: []string{repos}}})
	got := builtinNames(t, withRoots)
	for _, name := range []string{"read_file", "list_dir", "grep_files"} {
		if !got[name] {
			t.Errorf("%s missing even though files.roots names an existing directory", name)
		}
	}

	// A root that has not been synced yet is not a configuration error, but it
	// also must not produce tools that can only fail.
	missing := New(&config.Config{Files: config.Files{Roots: []string{filepath.Join(t.TempDir(), "not-yet")}}})
	if builtinNames(t, missing)["read_file"] {
		t.Error("tools were registered for a root that does not exist")
	}
}

// The code directories have to reach the sandbox too, or a script analysing the
// same tree the tools read would be blocked from reading it.
func TestFileRootsAreReadableByScripts(t *testing.T) {
	repos := filepath.Join(t.TempDir(), "repos")
	if err := os.MkdirAll(repos, 0o755); err != nil {
		t.Fatal(err)
	}
	e := New(&config.Config{
		Files:   config.Files{Roots: []string{repos}},
		Sandbox: config.Sandbox{ReadPaths: []string{"/opt/shared"}, WritePaths: []string{repos}},
	})
	pol := e.sandboxPolicy()

	var sawRoot, sawExplicit bool
	for _, p := range pol.ReadPaths {
		switch p {
		case repos:
			sawRoot = true
		case "/opt/shared":
			sawExplicit = true
		}
	}
	if !sawRoot {
		t.Error("files.roots did not reach the sandbox's readable paths")
	}
	if !sawExplicit {
		t.Error("folding in the roots dropped the explicitly configured read paths")
	}
	if len(pol.WritePaths) != 1 || pol.WritePaths[0] != repos {
		t.Errorf("write paths did not reach the policy: %v", pol.WritePaths)
	}
}

func TestGlobalRootsCannotBypassManagedProjectGrants(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	managed := filepath.Join(state, "code-projects", "snapshot-test")
	if err := os.MkdirAll(managed, 0700); err != nil {
		t.Fatal(err)
	}
	e := New(&config.Config{SourcePath: filepath.Join(state, "config.yaml"), Files: config.Files{Roots: []string{dir, state, managed}}})
	if !e.FileRoots().Empty() {
		t.Fatal("managed code exposed through global file tools")
	}
	if len(e.sandboxPolicy().ReadPaths) != 0 {
		t.Fatal("managed code inherited by scripts")
	}
}
