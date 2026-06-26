// Package sandbox runs semi-trusted code (skill scripts today, run_code later)
// inside a resource/isolation envelope. It offers two backends:
//
//   - "native"  — pure-Go, zero-dependency best-effort confinement (the default).
//     It scrubs the environment to a minimal allowlist (so host secrets in the
//     process env never reach the child), confines the working directory, caps
//     wall-clock time and captured output, kills the whole process group on
//     timeout (no orphaned grandchildren), and applies best-effort rlimits
//     (CPU time / process count / address space via the shell's ulimit).
//     SECURITY: this is HARDENING, not a security boundary — it does NOT restrict
//     filesystem access or the network. Only enable script execution for skills
//     you trust.
//   - "docker"  — optional strong isolation used when Docker is present and
//     enabled: an ephemeral container with no network, a read-only root
//     filesystem, memory/PID limits, and only the working directory mounted.
//
// Backend selection: an empty Policy.Backend picks "native" unless
// Policy.AllowDocker is set and a docker binary is on PATH, in which case
// "docker" is used. An explicit "docker" backend falls back to native (and
// notes it in the result) when docker is unavailable.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// Defaults applied when a Policy leaves a field zero.
const (
	DefaultTimeout   = 60 * time.Second
	DefaultMaxOutput = 8 << 10 // 8 KiB of combined stdout+stderr
	DefaultCPUSecs   = 30      // RLIMIT_CPU seconds (native, best-effort)
	DefaultMaxProcs  = 64      // RLIMIT_NPROC (native) / --pids-limit (docker)
	DefaultMemoryMB  = 512     // address space (native, Linux) / --memory (docker)
	DefaultImage     = "python:3.12-slim"
)

// Interpreters maps a script file extension to the interpreter that runs it,
// inside the sandbox. A file whose extension is absent here is executed directly
// (it must be +x with a shebang). Shared with the skill listing so both agree on
// what counts as runnable.
var Interpreters = map[string]string{
	".sh":   "sh",
	".bash": "bash",
	".py":   "python3",
	".js":   "node",
}

// Policy is the security/resource envelope for a run. The zero value is valid
// and yields the documented defaults via withDefaults.
type Policy struct {
	Backend     string        // "", "native", or "docker"
	AllowDocker bool          // permit auto-selecting the docker backend
	Network     bool          // docker: allow network (false ⇒ --network none)
	Timeout     time.Duration // wall-clock limit (both backends)
	MaxOutput   int           // captured bytes cap (both backends)
	CPUSeconds  int           // native: RLIMIT_CPU seconds (best-effort)
	MaxProcs    int           // docker: --pids-limit (native cannot cap per-sandbox)
	MemoryMB    int           // docker: --memory (native cannot cap reliably)
	Image       string        // docker image ("" ⇒ DefaultImage)
}

func (p Policy) withDefaults() Policy {
	if p.Timeout <= 0 {
		p.Timeout = DefaultTimeout
	}
	if p.MaxOutput <= 0 {
		p.MaxOutput = DefaultMaxOutput
	}
	if p.CPUSeconds <= 0 {
		p.CPUSeconds = DefaultCPUSecs
	}
	if p.MaxProcs <= 0 {
		p.MaxProcs = DefaultMaxProcs
	}
	if p.MemoryMB <= 0 {
		p.MemoryMB = DefaultMemoryMB
	}
	if p.Image == "" {
		p.Image = DefaultImage
	}
	return p
}

// Spec describes one execution: run RelFile (a path relative to Dir) under
// Interp, with Args appended and Env injected on top of a scrubbed base
// environment. An empty Interp executes RelFile directly (+x/shebang).
type Spec struct {
	Dir     string            // confinement root: the child's cwd / the docker mount
	Interp  string            // interpreter command, or "" to exec the file directly
	RelFile string            // script path relative to Dir (caller must confine it)
	Args    []string          // extra command-line arguments for the script
	Env     map[string]string // variables injected into the (scrubbed) child env
}

// Result is the outcome of a run. A non-zero ExitCode or TimedOut is NOT an
// error from Run's perspective — Run returns a non-nil error only when the
// command could not be started at all (missing interpreter, missing docker, …).
type Result struct {
	Output    string
	ExitCode  int
	Duration  time.Duration
	TimedOut  bool
	Truncated bool
	Backend   string // backend actually used ("native" or "docker")
}

// AuditEvent is one structured record of a sandbox run, handed to Audit.
type AuditEvent struct {
	Backend  string
	Dir      string
	Interp   string
	File     string
	Args     []string
	Duration time.Duration
	ExitCode int
	TimedOut bool
	Err      string
}

// Audit, when non-nil, receives one event per Run (success or failure). The app
// sets it to log executions for review (PLAN §8 risk 6 — audit log). nil is the
// safe default: silent.
var Audit func(AuditEvent)

// Run executes s under p, dispatching to the selected backend. It returns an
// error only for start failures; command exit status and timeouts are reported
// in the Result.
func Run(ctx context.Context, p Policy, s Spec) (Result, error) {
	p = p.withDefaults()

	backend := p.Backend
	switch backend {
	case "docker":
		if !dockerAvailable() {
			backend = "native" // explicit request, but docker missing → degrade
		}
	case "native":
		// keep
	default: // "" → auto
		if p.AllowDocker && dockerAvailable() {
			backend = "docker"
		} else {
			backend = "native"
		}
	}

	start := time.Now()
	var res Result
	var err error
	if backend == "docker" {
		res, err = runDocker(ctx, p, s)
	} else {
		res, err = runNative(ctx, p, s)
	}
	res.Backend = backend
	res.Duration = time.Since(start)

	if Audit != nil {
		ev := AuditEvent{
			Backend:  backend,
			Dir:      s.Dir,
			Interp:   s.Interp,
			File:     s.RelFile,
			Args:     s.Args,
			Duration: res.Duration,
			ExitCode: res.ExitCode,
			TimedOut: res.TimedOut,
		}
		if err != nil {
			ev.Err = err.Error()
		}
		Audit(ev)
	}
	return res, err
}

// capture runs cmd with combined stdout+stderr buffered and capped at max bytes.
// It returns the (possibly truncated) output, whether truncation happened, the
// process exit code, and — only when the command failed to start (not merely a
// non-zero exit) — a start error.
func capture(cmd *exec.Cmd, max int) (out string, truncated bool, code int, startErr error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	out = buf.String()
	if max > 0 && len(out) > max {
		out = out[:max] + "\n…（输出已截断）"
		truncated = true
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return out, truncated, ee.ExitCode(), nil
		}
		return out, truncated, -1, runErr
	}
	return out, truncated, 0, nil
}
