package sandbox

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// dockerOnce caches the docker-availability probe (a PATH lookup) for the
// process lifetime — the docker binary does not appear or vanish mid-run.
var (
	dockerOnce sync.Once
	dockerOK   bool
)

func dockerAvailable() bool {
	dockerOnce.Do(func() {
		_, err := exec.LookPath("docker")
		dockerOK = err == nil
	})
	return dockerOK
}

// DockerAvailable reports whether a docker binary is on PATH, so callers (e.g.
// the web UI) can warn before selecting the docker backend. Uses a fresh PATH
// lookup rather than the cached probe so it reflects the current environment.
func DockerAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// dockerArgs renders the full `docker …` argv for one run. It is separate from
// runDocker so the envelope can be asserted without a docker daemon.
func dockerArgs(p Policy, s Spec) []string {
	mem := strconv.Itoa(p.MemoryMB) + "m"
	// The mode governs docker exactly as it governs the os backend: the one
	// word an operator sets has to mean the same thing whichever backend ends up
	// running, or picking a stronger backend would silently change the envelope.
	mount := s.Dir + ":/work"
	if !p.Mode.CanWrite() {
		mount += ":ro"
	}
	args := []string{
		"run", "--rm", "-i",
		"--workdir", "/work",
		"--volume", mount,
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,size=64m",
		"--pids-limit", strconv.Itoa(p.MaxProcs),
		"--memory", mem,
		"--memory-swap", mem, // == memory ⇒ no swap
		"--env", "HOME=/tmp",
	}
	if !p.Mode.CanNetwork() {
		args = append(args, "--network", "none")
	}
	args = append(args, dockerUser()...)
	for k, v := range s.Env {
		args = append(args, "--env", k+"="+v)
	}
	args = append(args, p.Image)

	rel := "/work/" + filepath.ToSlash(s.RelFile)
	if s.Interp != "" {
		args = append(args, s.Interp, rel)
	} else {
		args = append(args, rel)
	}
	args = append(args, s.Args...)
	return args
}

// runDocker executes the script inside an ephemeral container. The working
// directory is bind-mounted at /work (the only writable host path); the root
// filesystem is read-only, /tmp is a small tmpfs, and — unless the mode allows
// the network — the container has no network at all. Under read-only the
// workspace mount itself is read-only too. Memory and PID caps come from the
// policy.
func runDocker(ctx context.Context, p Policy, s Spec) (Result, error) {
	args := dockerArgs(p, s)

	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return cmd.Process.Kill() // --rm reaps the container when the CLI dies
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second

	out, truncated, code, startErr := capture(cmd, p.MaxOutput)
	res := Result{
		Output:    out,
		ExitCode:  code,
		Truncated: truncated,
		TimedOut:  ctx.Err() == context.DeadlineExceeded,
	}
	return res, startErr
}
