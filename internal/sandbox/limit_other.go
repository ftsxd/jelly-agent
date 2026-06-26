//go:build !unix

package sandbox

import "os/exec"

// On non-Unix platforms we cannot create a process group or apply ulimit-style
// rlimits, so confinement degrades to the wall-clock timeout and output cap.

func configureProc(*exec.Cmd) {}

func killProc(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func ulimitPrelude(Policy) string { return "" }

func dockerUser() []string { return nil }
