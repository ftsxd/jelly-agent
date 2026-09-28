package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
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
	if p.Strict {
		args = append(args, "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pull", "never")
	}
	args = append(args, dockerUser()...)
	for k, v := range s.Env {
		if p.Strict {
			// Docker reads the value from its CLI environment; never publish
			// injected credentials in process arguments visible to host tools.
			args = append(args, "--env", k)
		} else {
			args = append(args, "--env", k+"="+v)
		}
	}
	args = append(args, p.Image)
	if len(s.Argv) > 0 {
		return append(args, s.Argv...)
	}

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
	if s.Managed != nil {
		return runManagedDocker(ctx, p, s)
	}
	args := dockerArgs(p, s)
	var container string
	if p.Strict {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return Result{ExitCode: -1}, err
		}
		container = "jelly-exec-" + hex.EncodeToString(id[:])
		args = append([]string{"run", "--name", container}, args[1:]...)
		// Also clean up after the CLI has stopped: cancellation can race
		// container creation, before the first removal sees the name.
		defer removeContainer(container)
	}

	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", args...)
	if p.Strict {
		cmd.Env = os.Environ()
		for k, v := range s.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Cancel = func() error {
		if container != "" {
			removeContainer(container)
		}
		if cmd.Process != nil {
			return cmd.Process.Kill() // removal above terminates the container
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second

	res, startErr := capture(cmd, p.MaxOutput)
	res.TimedOut = ctx.Err() == context.DeadlineExceeded
	res.Cancelled = ctx.Err() == context.Canceled
	return res, startErr
}

func removeContainer(name string) {
	cleanup, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	_ = exec.CommandContext(cleanup, "docker", "rm", "-f", name).Run()
}
