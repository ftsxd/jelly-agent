//go:build unix

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// configureProc puts the child in its own process group so killProc can signal
// the whole tree (the script plus anything it spawns), not just the leader.
func configureProc(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProc force-kills the child's process group. A negative PID targets the
// group whose ID equals the child PID (set up by Setpgid above).
func killProc(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// ulimitPrelude builds a POSIX-sh snippet applied before the script runs. It
// caps only CPU time — the one rlimit that is both portable and safe here:
//
//   - RLIMIT_NPROC (ulimit -u) counts ALL processes of the real user, not just
//     this sandbox, so a small value fork-bombs the host's own session.
//   - RLIMIT_AS (ulimit -v) caps address space, which routinely breaks runtimes
//     (node, threaded Python) that reserve large virtual ranges, and is
//     unsupported on macOS.
//
// Memory and PID caps are therefore enforced only by the docker backend
// (--memory / --pids-limit), where they apply per-container. The guard
// 2>/dev/null keeps the run going if even -t is unavailable.
func ulimitPrelude(p Policy) string {
	if p.CPUSeconds <= 0 {
		return ""
	}
	return "ulimit -t " + strconv.Itoa(p.CPUSeconds) + " 2>/dev/null;"
}

// dockerUser maps the container process to the current uid:gid so files written
// into the mounted working directory stay owned by the host user.
func dockerUser() []string {
	return []string{"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}
}
