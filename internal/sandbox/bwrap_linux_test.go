package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A symlinked read path must show up in the sandbox as the symlink, not just
// as its target: merged-/usr hosts reach the dynamic loader through /lib64.
func TestBwrapArgvRecreatesSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "usr-lib")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "lib")
	if err := os.Symlink("usr-lib", link); err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(bwrapArgv(Policy{Mode: ModeReadOnly, ReadPaths: []string{link}}, root), " ")
	if !strings.Contains(argv, "--symlink usr-lib "+link) {
		t.Fatalf("symlink not recreated: %s", argv)
	}
	if c := canonical(real); !strings.Contains(argv, "--ro-bind-try "+c+" "+c) {
		t.Fatalf("target not bound: %s", argv)
	}
}

// The generic executor's layout (Strict, offline) must be able to start a
// dynamically linked program and must not see the agent's own environment.
func TestBwrapStrictRunsCommandsInRealSandbox(t *testing.T) {
	if !bwrapAvailable() {
		t.Skip(bwrapUnavailable())
	}
	t.Setenv("JELLY_BWRAP_TEST_SECRET", "must-not-leak")
	p := Policy{Strict: true, Backend: "os", Mode: ModeWorkspace}
	if landlockSatisfiesStrict(p.withDefaults()) {
		t.Skip("Landlock is chosen over bwrap here")
	}
	// canonical("/proc/self") in this process is /proc/<our pid>; that path
	// must not exist inside the sandbox's own PID namespace.
	script := fmt.Sprintf("echo ran; cat /proc/%d/environ 2>/dev/null | tr '\\0' '\\n'", os.Getpid())
	res, err := Run(t.Context(), p, Spec{Dir: t.TempDir(), Argv: []string{"sh", "-c", script}})
	if err != nil || res.ExitCode != 0 || !strings.HasPrefix(res.Stdout, "ran") {
		t.Fatalf("strict bwrap run failed: %+v %v", res, err)
	}
	if strings.Contains(res.Stdout, "must-not-leak") {
		t.Fatalf("agent environment visible in sandbox: %s", res.Stdout)
	}
}

// /proc and /dev are synthesised by bwrap; none of the real ones may be bound
// back in — /proc/self resolves to the agent's own /proc/<pid>.
func TestBwrapArgvNeverBindsProcOrDev(t *testing.T) {
	argv := bwrapArgv(Policy{Strict: true, Mode: ModeWorkspace}, t.TempDir())
	for i, a := range argv {
		if a != "--ro-bind-try" && a != "--bind-try" && a != "--symlink" {
			continue
		}
		if dst := argv[i+2]; bwrapSynthetic(dst) {
			t.Fatalf("%s %s %s overlays the synthetic tree", a, argv[i+1], dst)
		}
	}
}
