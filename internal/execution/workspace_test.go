package execution

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateWorkspaceCleanupRecoversDirectoryPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "execution")
	private := filepath.Join(dir, ".kube")
	if err := os.MkdirAll(private, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "config"), []byte("test credential"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0000); err != nil {
		t.Fatal(err)
	}
	if err := cleanupWorkspace(root, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("credential workspace remains: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("cleanup followed a symlink outside its root", err)
	}
}
