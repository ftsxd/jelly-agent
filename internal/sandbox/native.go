package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// runNative executes the script with best-effort process confinement: a scrubbed
// environment, a confined cwd, a wall-clock timeout that kills the whole process
// group, and (on Unix) a CPU-time rlimit. It does NOT cap memory, PIDs, the
// filesystem, or the network — see the package doc for the security caveats.
func runNative(ctx context.Context, p Policy, s Spec) (Result, error) {
	abs := filepath.Join(s.Dir, filepath.FromSlash(s.RelFile))

	// The literal command we want to run (interpreter + script, or the script
	// directly when no interpreter is mapped).
	var target []string
	if s.Interp != "" {
		target = append([]string{s.Interp, abs}, s.Args...)
	} else {
		target = append([]string{abs}, s.Args...)
	}

	// On Unix, wrap in `sh -c 'ulimit …; exec "$@"' sh <target…>` so the rlimits
	// apply to the script and everything it spawns. With no prelude (non-Unix),
	// run the target directly.
	name, args := target[0], target[1:]
	if prelude := ulimitPrelude(p); prelude != "" {
		name = "sh"
		args = append([]string{"-c", prelude + ` exec "$@"`, "sh"}, target...)
	}

	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = s.Dir
	cmd.Env = scrubEnv(s.Dir, s.Env)
	configureProc(cmd)          // process-group leader on Unix
	cmd.Cancel = func() error { // ctx timeout/cancel → kill the whole group
		killProc(cmd)
		return nil
	}
	cmd.WaitDelay = 2 * time.Second // force-close inherited pipes shortly after a kill

	out, truncated, code, startErr := capture(cmd, p.MaxOutput)
	res := Result{
		Output:    out,
		ExitCode:  code,
		Truncated: truncated,
		TimedOut:  ctx.Err() == context.DeadlineExceeded,
	}
	return res, startErr
}

// scrubEnv builds the child environment from scratch: a minimal allowlist plus
// the caller's injected variables. Critically, it does NOT inherit the host
// process environment, so secrets the server holds (API keys, tokens) never leak
// into sandboxed code. HOME and TMPDIR point at the confined dir.
func scrubEnv(dir string, inj map[string]string) []string {
	env := []string{
		"PATH=" + pathEnv(),
		"HOME=" + dir,
		"TMPDIR=" + dir,
		"LANG=C.UTF-8",
	}
	for k, v := range inj {
		env = append(env, k+"="+v)
	}
	return env
}

func pathEnv() string {
	if p := os.Getenv("PATH"); p != "" {
		return p // keep the host PATH so interpreters (python3/node) resolve
	}
	return "/usr/local/bin:/usr/bin:/bin"
}
