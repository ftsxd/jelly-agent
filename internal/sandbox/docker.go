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

// runDocker executes the script inside an ephemeral container. The working
// directory is bind-mounted at /work (the only writable host path); the root
// filesystem is read-only, /tmp is a small tmpfs, and — unless Policy.Network is
// set — the container has no network at all. Memory and PID caps come from the
// policy.
func runDocker(ctx context.Context, p Policy, s Spec) (Result, error) {
	mem := strconv.Itoa(p.MemoryMB) + "m"
	args := []string{
		"run", "--rm", "-i",
		"--workdir", "/work",
		"--volume", s.Dir + ":/work",
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,size=64m",
		"--pids-limit", strconv.Itoa(p.MaxProcs),
		"--memory", mem,
		"--memory-swap", mem, // == memory ⇒ no swap
		"--env", "HOME=/tmp",
	}
	if !p.Network {
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
