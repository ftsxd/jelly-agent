package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot builds a root directory that looks like a synced project.
func repoRoot(t *testing.T) (root string, secretDir string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "repos")
	if err := os.MkdirAll(filepath.Join(root, "project-a", "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "project-a", "internal", "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Somewhere outside the root, standing in for the agent's own config.
	secretDir = filepath.Join(base, "private")
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "config.yaml"), []byte("api_key: TOPSECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, secretDir
}

func TestRootsResolvesInsideAndRefusesOutside(t *testing.T) {
	root, secretDir := repoRoot(t)
	r := NewRoots([]string{root})
	if r.Empty() {
		t.Fatal("root should be usable")
	}

	// Relative to the root — the shape the model is told to use.
	got, err := r.Resolve("project-a/internal/x.go")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join("project-a", "internal", "x.go")) {
		t.Fatalf("resolved to %q", got)
	}

	for _, bad := range []string{
		"../private/config.yaml",
		"project-a/../../private/config.yaml",
		filepath.Join(secretDir, "config.yaml"), // absolute, outside
		"/etc/passwd",
	} {
		if _, err := r.Resolve(bad); err == nil {
			t.Errorf("escaped the root: %q", bad)
		}
	}
}

// A synced repository is content someone else wrote. A symlink checked into it
// is the obvious way out, and it only fails to work if containment is decided
// after resolution rather than before.
func TestRootsRefusesSymlinkOutOfTheRoot(t *testing.T) {
	root, secretDir := repoRoot(t)
	link := filepath.Join(root, "project-a", "escape")
	if err := os.Symlink(secretDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r := NewRoots([]string{root})

	if _, err := r.Resolve("project-a/escape/config.yaml"); err == nil {
		t.Fatal("followed a symlink out of the root")
	}
	if _, err := r.Resolve("project-a/escape"); err == nil {
		t.Fatal("resolved a symlinked directory pointing outside the root")
	}

	// A symlink that stays inside is fine — confinement, not paranoia.
	inner := filepath.Join(root, "project-a", "alias")
	if err := os.Symlink(filepath.Join(root, "project-a", "internal"), inner); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := r.Resolve("project-a/alias/x.go"); err != nil {
		t.Fatalf("an in-root symlink was refused: %v", err)
	}
}

func TestRootsEmptyWhenNothingUsable(t *testing.T) {
	if !NewRoots(nil).Empty() {
		t.Error("nil roots must be empty")
	}
	if !NewRoots([]string{"", "   "}).Empty() {
		t.Error("blank roots must be empty")
	}
	// A directory that has not been synced yet is dropped, not fatal.
	if !NewRoots([]string{filepath.Join(t.TempDir(), "not-yet")}).Empty() {
		t.Error("a missing root must be dropped")
	}
	// A file is not a root.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !NewRoots([]string{f}).Empty() {
		t.Error("a file must not be usable as a root")
	}
	if _, err := (Roots{}).Resolve("anything"); err == nil {
		t.Error("resolving with no roots must fail")
	}
}

// With several roots the model needs to be able to name which project it means,
// and the paths it reads back have to be the paths it can pass in again.
func TestRootsDisplayRoundTrips(t *testing.T) {
	rootA, _ := repoRoot(t)
	rootB := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(filepath.Join(rootB, "project-b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "project-b", "y.go"), []byte("package y\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	single := NewRoots([]string{rootA})
	abs, err := single.Resolve("project-a/internal/x.go")
	if err != nil {
		t.Fatal(err)
	}
	if d := single.Display(abs); d != "project-a/internal/x.go" {
		t.Fatalf("single-root display = %q", d)
	}
	if _, err := single.Resolve(single.Display(abs)); err != nil {
		t.Fatalf("display did not round trip: %v", err)
	}

	multi := NewRoots([]string{rootA, rootB})
	absB, err := multi.Resolve("project-b/y.go")
	if err != nil {
		t.Fatalf("second root unreachable: %v", err)
	}
	if d := multi.Display(absB); !strings.HasPrefix(d, filepath.Base(rootB)+"/") {
		t.Fatalf("multi-root display must name its root, got %q", d)
	}
}

// scopedRepo builds a miniature monorepo: two services, a shared directory, and
// a secret the scope must keep out of reach.
func scopedRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"services/order", "services/pay", "common", "ad"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(d), "main.go"), []byte("package "+filepath.Base(d)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Resolve, because that is what the tools see: every path handed to
	// AllowsDir/IsNavigation has already been through Resolve or a walk rooted
	// at the resolved root. On macOS t.TempDir() is a symlink.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestScopedRootRejectsPathsOutsideTheConfiguredDirectories(t *testing.T) {
	root := scopedRepo(t)
	r := NewScopedRoot(root, []string{"services/order", "common"})

	for _, ok := range []string{"services/order", "services/order/main.go", "common/main.go"} {
		if _, err := r.Resolve(ok); err != nil {
			t.Fatalf("configured path rejected: %s: %v", ok, err)
		}
	}
	for _, blocked := range []string{"services/pay/main.go", "ad/main.go", "services/pay"} {
		if _, err := r.Resolve(blocked); err == nil {
			t.Fatalf("path outside the configured directories was allowed: %s", blocked)
		}
	}
}

// "services/order" must not also allow "services/orders": a shared name prefix
// is not containment.
func TestScopedRootDoesNotLeakIntoSiblingsSharingANamePrefix(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"services/order", "services/orders"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(d), "x.go"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := NewScopedRoot(root, []string{"services/order"})
	if _, err := r.Resolve("services/orders/x.go"); err == nil {
		t.Fatal("sibling sharing a name prefix was treated as in scope")
	}
}

// A repository is attacker-influenced content: a checked-in symlink pointing at
// a directory outside the scope must not become a way in.
func TestScopedRootResolvesSymlinksBeforeCheckingScope(t *testing.T) {
	root := scopedRepo(t)
	link := filepath.Join(root, "services", "order", "shortcut")
	if err := os.Symlink(filepath.Join(root, "services", "pay"), link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	r := NewScopedRoot(root, []string{"services/order"})
	if _, err := r.Resolve("services/order/shortcut/main.go"); err == nil {
		t.Fatal("a symlink walked out of the configured scope")
	}
}

// The model must read back repo-relative paths it can pass straight back in,
// even though the project exposes several directories.
func TestScopedRootKeepsPathsRepoRelative(t *testing.T) {
	root := scopedRepo(t)
	r := NewScopedRoot(root, []string{"services/order", "common"})
	abs, err := r.Resolve("services/order/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Display(abs); got != "services/order/main.go" {
		t.Fatalf("display path is not repo-relative: %q", got)
	}
}

func TestScopedRootTreatsAncestorsAsNavigationOnly(t *testing.T) {
	root := scopedRepo(t)
	r := NewScopedRoot(root, []string{"services/order"})
	services := filepath.Join(root, "services")
	if r.AllowsDir(services) {
		t.Fatal("an ancestor of the scope must not itself be in scope")
	}
	if !r.IsNavigation(services) {
		t.Fatal("an ancestor of the scope must be traversable")
	}
	if got := r.NavigationChildren(services); len(got) != 1 || got[0] != "order" {
		t.Fatalf("navigation exposed more than the way down: %v", got)
	}
	if !r.IsNavigation(root) {
		t.Fatal("the repository root must be traversable")
	}
}

// Empty path means "everywhere I may look", which for a scoped project is the
// configured directories — not the repository they sit in.
func TestScopedRootSearchesOnlyTheConfiguredDirectories(t *testing.T) {
	root := scopedRepo(t)
	r := NewScopedRoot(root, []string{"services/order", "common"})
	targets, err := r.ResolveOrRoots("")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected the two configured directories, got %v", targets)
	}
	for _, target := range targets {
		if !r.AllowsDir(target) {
			t.Fatalf("search target outside the scope: %s", target)
		}
	}
}

// A whole-repo project and every project created before scoping existed must
// keep reaching the entire tree.
func TestUnscopedRootIsUnchanged(t *testing.T) {
	root := scopedRepo(t)
	for name, r := range map[string]Roots{
		"nil prefixes": NewScopedRoot(root, nil),
		"dot":          NewScopedRoot(root, []string{"."}),
		"plain":        NewRoots([]string{root}),
	} {
		if _, err := r.Resolve("services/pay/main.go"); err != nil {
			t.Fatalf("%s: whole-repo access lost: %v", name, err)
		}
		if !r.AllowsDir(filepath.Join(root, "ad")) {
			t.Fatalf("%s: unscoped root refused a directory", name)
		}
	}
}
