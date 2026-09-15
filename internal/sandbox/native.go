package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// runNative executes the script with best-effort process confinement: a scrubbed
// environment, a confined cwd, a wall-clock timeout that kills the whole process
// group, and (on Unix) a CPU-time rlimit. It does NOT cap memory, PIDs, the
// filesystem, or the network — see the package doc for the security caveats.
func runNative(ctx context.Context, p Policy, s Spec) (Result, error) {
	return runLocal(ctx, p, s, nil)
}

// runLocal runs s directly on this host, optionally behind a confinement wrapper
// supplied by the "os" backend (macOS sandbox-exec, or the Linux Landlock
// helper). The wrapper must be outermost: it is what applies the policy, and
// everything it spawns — including the ulimit shell — inherits it.
//
// The resulting argv is, from the outside in:
//
//	<prefix…> sh -c 'ulimit -t N; exec "$@"' sh <interp> <script> <args…>
func runLocal(ctx context.Context, p Policy, s Spec, prefix []string) (Result, error) {
	argv := targetArgv(s)
	// The rlimit wrapper needs a shell. A minimal runtime image may not have
	// one, and wrapping in a `sh` that does not exist would turn every run into
	// a start failure — the CPU cap is worth having, but not at that price.
	if prelude := ulimitPrelude(p); prelude != "" && hasShell() {
		// Wrap in `sh -c 'ulimit …; exec "$@"' sh <target…>` so the rlimit
		// applies to the script and everything it spawns.
		argv = append([]string{"sh", "-c", prelude + ` exec "$@"`, "sh"}, argv...)
	}
	if len(prefix) > 0 {
		argv = append(append([]string{}, prefix...), argv...)
	}

	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
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

// targetArgv is the literal command we want to run: the interpreter plus the
// script, or the script directly when no interpreter is mapped.
func targetArgv(s Spec) []string {
	abs := filepath.Join(s.Dir, filepath.FromSlash(s.RelFile))
	if s.Interp != "" {
		return append([]string{s.Interp, abs}, s.Args...)
	}
	return append([]string{abs}, s.Args...)
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

var (
	shellOnce sync.Once
	shellOK   bool
)

// hasShell reports whether a POSIX sh is on PATH.
func hasShell() bool {
	shellOnce.Do(func() {
		_, err := exec.LookPath("sh")
		shellOK = err == nil
	})
	return shellOK
}

func pathEnv() string {
	if p := os.Getenv("PATH"); p != "" {
		return p // keep the host PATH so interpreters (python3/node) resolve
	}
	return "/usr/local/bin:/usr/bin:/bin"
}

// canonical resolves symlinks, because both confinement layers match the real
// path: /tmp and /var are symlinks into /private on macOS, and Landlock resolves
// its path rules at setup time. A policy written against the unresolved path
// matches nothing.
func canonical(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
