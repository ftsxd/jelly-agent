package execution

import (
	"os"
	"path/filepath"
)

// Hold the root opened before the child starts. Permission recovery never
// follows a child-created symlink outside the private execution workspace.
func cleanupWorkspace(root *os.Root, dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	if err := restoreWorkspaceDirs(root, "."); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func restoreWorkspaceDirs(root *os.Root, name string) error {
	if err := root.Chmod(name, 0700); err != nil {
		return err
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := restoreWorkspaceDirs(root, filepath.Join(name, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
