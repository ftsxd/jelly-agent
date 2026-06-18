package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeDirSkill creates a directory-form skill with a SKILL.md and the given
// extra files (name→content), marking .sh files executable.
func writeDirSkill(t *testing.T, st *Store, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(st.Dir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: "+name+"\ndescription: t\nenabled: true\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	for n, c := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(n, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunScriptInjectsEnv(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	writeDirSkill(t, st, "greeter", map[string]string{
		"run.sh": "#!/bin/sh\necho \"hi $WHO\"",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := st.RunScript(ctx, "greeter", "run.sh", nil, map[string]string{"WHO": "jelly"})
	if err != nil {
		t.Fatalf("run: %v (out=%q)", err, out)
	}
	if !strings.Contains(out, "hi jelly") {
		t.Fatalf("env not injected, out=%q", out)
	}

	// Scripts() surfaces the runnable file.
	if got := st.Scripts("greeter"); len(got) != 1 || got[0] != "run.sh" {
		t.Fatalf("Scripts = %v", got)
	}
}

func TestRunScriptRejectsTraversal(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	writeDirSkill(t, st, "s", nil)
	ctx := context.Background()
	if _, err := st.RunScript(ctx, "s", "../../etc/passwd", nil, nil); err == nil {
		t.Fatal("expected path-traversal rejection")
	}
	if _, err := st.RunScript(ctx, "s", "nope.sh", nil, nil); err == nil {
		t.Fatal("expected missing-script error")
	}
}

func TestRunScriptTimeout(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	writeDirSkill(t, st, "slow", map[string]string{"loop.sh": "#!/bin/sh\nsleep 5"})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := st.RunScript(ctx, "slow", "loop.sh", nil, nil); err == nil {
		t.Fatal("expected timeout error")
	}
}
