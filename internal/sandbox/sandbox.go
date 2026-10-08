// Package sandbox runs semi-trusted code (skill scripts today, run_code later)
// inside a resource/isolation envelope. Three backends, weakest to strongest:
//
//   - "native"  — pure-Go, zero-dependency best-effort confinement. It scrubs the
//     environment to a minimal allowlist (so host secrets in the process env
//     never reach the child), confines the working directory, caps wall-clock
//     time and captured output, kills the whole process group on timeout (no
//     orphaned grandchildren), and applies a best-effort CPU rlimit.
//     SECURITY: this is HARDENING, not a security boundary — it does NOT restrict
//     filesystem access or the network.
//   - "os"      — everything native does, plus a real confinement layer taken from
//     the operating system itself: no daemon, no container, no image pull. macOS
//     uses Seatbelt (sandbox-exec); Linux uses Landlock, applied by re-executing
//     this binary's hidden `__sandbox` subcommand, which locks itself down and
//     then execs the target. Reads are limited to system paths plus the
//     workspace, writes to the workspace, and the network is denied unless the
//     mode allows it. This is the default wherever the platform supports it.
//   - "docker"  — the strongest and the heaviest: an ephemeral container with no
//     network, a read-only root filesystem, memory/PID limits, and only the
//     working directory mounted. Needs a docker binary (and, in a containerized
//     deployment, a mounted docker socket — which is itself a privilege
//     handover, so it is opt-in via Policy.AllowDocker).
//
// What the "os" backend actually enforces depends on the platform and, on Linux,
// on the running kernel. It never pretends: whatever could not be enforced is
// reported in Result.Degraded and in the audit event.
//
// Policy.ReadPaths and Policy.WritePaths widen those columns deliberately: a
// script that syncs repositories has to write somewhere that is not its own
// directory, and saying so in config beats the alternative of switching the
// whole policy off for that one skill.
//
//	平台 / 内核        文件读      文件写      网络
//	macOS Seatbelt     允许列表    工作目录    拒绝（mode 放开时除外）
//	Linux Landlock ≥4  允许列表    工作目录    拒绝 TCP（UDP 不在 Landlock v4 管辖内）
//	Linux Landlock 1-3 允许列表    工作目录    不受限 → Degraded 说明原因
//	Linux Landlock 0   —           —           — → 退回 native
//
// Backend selection: an empty Policy.Backend picks docker when AllowDocker is set
// and docker is present, else "os" when the platform supports it, else "native".
// An explicitly requested backend that is unavailable degrades one step rather
// than failing the run, and the degradation is always visible in the Result.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Defaults applied when a Policy leaves a field zero.
const (
	DefaultTimeout   = 60 * time.Second
	DefaultMaxOutput = 8 << 10 // 8 KiB of combined stdout+stderr
	DefaultCPUSecs   = 30      // RLIMIT_CPU seconds (native/os, best-effort)
	DefaultMaxProcs  = 64      // docker: --pids-limit
	DefaultMemoryMB  = 512     // docker: --memory
	DefaultImage     = "python:3.12-slim"
	DefaultMode      = ModeWorkspace
)

// Mode is the confinement intent, expressed as one word instead of a pile of
// knobs — the abstraction Codex CLI settled on (read-only / workspace-write /
// danger-full-access) and the one thing every caller actually reasons about.
// The resource caps in Policy are orthogonal and still apply.
type Mode string

const (
	// ModeReadOnly forbids writing anywhere and forbids the network. Diagnostics
	// that only read and report belong here.
	ModeReadOnly Mode = "read-only"
	// ModeWorkspace (the default) allows writes inside the script's own
	// directory and forbids the network.
	ModeWorkspace Mode = "workspace"
	// ModeWorkspaceNet is ModeWorkspace with the network allowed — the mode most
	// ops scripts need, since they query Prometheus / the K8s API / an internal
	// endpoint. It is deliberately a separate word so that granting egress is an
	// explicit decision rather than a forgotten default.
	ModeWorkspaceNet Mode = "workspace-net"
	// ModeFull applies no isolation at all (native hardening only). Escape
	// hatch; every run under it is audited as unconfined.
	ModeFull Mode = "full"
)

// Modes lists every valid mode, weakest confinement last, for config validation
// and for the web UI's picker.
var Modes = []Mode{ModeReadOnly, ModeWorkspace, ModeWorkspaceNet, ModeFull}

// Valid reports whether m is one of the defined modes.
func (m Mode) Valid() bool {
	for _, x := range Modes {
		if m == x {
			return true
		}
	}
	return false
}

// CanWrite reports whether the mode permits writing to the workspace directory.
func (m Mode) CanWrite() bool { return m != ModeReadOnly }

// CanNetwork reports whether the mode permits network access.
func (m Mode) CanNetwork() bool { return m == ModeWorkspaceNet || m == ModeFull }

// Confines reports whether the mode asks for any isolation at all.
func (m Mode) Confines() bool { return m != ModeFull }

// AtMost clamps m to other, returning whichever is the more restrictive of the
// two. It is how a per-skill mode is applied: a skill may tighten its own
// envelope but never loosen the one the operator configured.
func (m Mode) AtMost(other Mode) Mode {
	if rank(m) <= rank(other) {
		return m
	}
	return other
}

// rank orders modes from most to least restrictive.
func rank(m Mode) int {
	switch m {
	case ModeReadOnly:
		return 0
	case ModeWorkspace:
		return 1
	case ModeWorkspaceNet:
		return 2
	default: // ModeFull and anything unrecognized
		return 3
	}
}

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
	// Strict refuses unavailable/weaker isolation before the command starts.
	Strict      bool
	Mode        Mode          // confinement intent ("" ⇒ derived from Network, else DefaultMode)
	Backend     string        // "", "native", "os", or "docker"
	AllowDocker bool          // permit auto-selecting the docker backend
	Network     bool          // legacy switch, honored only when Mode is empty
	ReadPaths   []string      // extra host paths the script may read (os backend)
	WritePaths  []string      // extra host paths the script may write (os backend)
	Timeout     time.Duration // wall-clock limit (all backends)
	MaxOutput   int           // captured bytes cap (all backends)
	CPUSeconds  int           // native/os: RLIMIT_CPU seconds (best-effort)
	MaxProcs    int           // docker: --pids-limit (native/os cannot cap per-sandbox)
	MemoryMB    int           // docker: --memory (native/os cannot cap reliably)
	Image       string        // docker image ("" ⇒ DefaultImage)
}

func (p Policy) withDefaults() Policy {
	if p.Mode == "" {
		// Legacy config that only knew the Network boolean still means what it
		// used to mean.
		if p.Network {
			p.Mode = ModeWorkspaceNet
		} else {
			p.Mode = DefaultMode
		}
	}
	if !p.Mode.Valid() {
		p.Mode = DefaultMode
	}
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

// EffectiveMode is the mode this policy actually runs under, with the defaults
// and the legacy Network flag already resolved. Callers that clamp a per-skill
// mode against the operator's must compare with this, not with the raw field —
// an empty Policy.Mode means "the default", not "no confinement".
func (p Policy) EffectiveMode() Mode { return p.withDefaults().Mode }

// Spec describes one execution: run RelFile (a path relative to Dir) under
// Interp, with Args appended and Env injected on top of a scrubbed base
// environment. An empty Interp executes RelFile directly (+x/shebang).
type Spec struct {
	// Argv executes literal arguments instead of a script when set.
	Argv    []string
	Dir     string            // confinement root: the child's cwd / the writable path / the docker mount
	Interp  string            // interpreter command, or "" to exec the file directly
	RelFile string            // script path relative to Dir (caller must confine it)
	Args    []string          // extra command-line arguments for the script
	Env     map[string]string // variables injected into the (scrubbed) child env
	Secrets map[string]string // redact-only values (e.g. inline kubeconfig tokens)
	// Managed is server-owned lifecycle authority, never a model argument.
	Managed *ManagedContainer
}

// Result is the outcome of a run. A non-zero ExitCode or TimedOut is NOT an
// error from Run's perspective. Run returns an error on start or I/O collection
// failure; Started distinguishes those cases so a write is never blindly retried.
type Result struct {
	Started   bool // child was started, even if waiting/output collection failed
	Output    string
	Stdout    string
	Stderr    string
	ExitCode  int
	Duration  time.Duration
	TimedOut  bool
	Cancelled bool
	Truncated bool
	Backend   string // backend actually used ("native", "os", or "docker")
	Mode      Mode   // mode actually applied
	Degraded  string // non-empty when the envelope is weaker than asked for, and why
}

// AuditEvent is one structured record of a sandbox run, handed to Audit.
type AuditEvent struct {
	Backend   string
	Mode      Mode
	Degraded  string
	Dir       string
	Interp    string
	File      string
	Args      []string
	Duration  time.Duration
	ExitCode  int
	TimedOut  bool
	Cancelled bool
	Err       string
}

// Audit, when non-nil, receives one event per Run (success or failure). The app
// sets it to log executions for review (PLAN §8 risk 6 — audit log). nil is the
// safe default: silent.
var Audit func(AuditEvent)

// selectBackend resolves the configured backend against what this host actually
// offers, returning the backend to use and a human-readable note when the answer
// is weaker than what was asked for. Degrading beats failing the run, but a
// silent degrade would be a lie — hence the note.
func selectBackend(p Policy) (backend, degraded string) {
	if !p.Mode.Confines() {
		// Nothing to enforce; every backend would be doing theatre.
		return "native", "mode=full：本次执行未施加任何文件系统/网络隔离"
	}
	switch p.Backend {
	case "docker":
		if dockerAvailable() {
			return "docker", ""
		}
		if osAvailable() {
			return "os", "docker 不可用（PATH 上没有 docker），已退回 os 后端"
		}
		return "native", "docker 与 os 后端都不可用（" + osUnavailable() + "），已退回 native：文件系统与网络不受限"
	case "os":
		if osAvailable() {
			return "os", ""
		}
		return "native", "os 后端不可用（" + osUnavailable() + "），已退回 native：文件系统与网络不受限"
	case "native":
		return "native", "backend=native：文件系统与网络不受限"
	default: // "" → auto
		if p.AllowDocker && dockerAvailable() {
			return "docker", ""
		}
		if osAvailable() {
			return "os", ""
		}
		return "native", "os 后端不可用（" + osUnavailable() + "），已退回 native：文件系统与网络不受限"
	}
}

// OSSandboxAvailable reports whether this host can actually enforce the "os"
// backend, so the UI can tell an operator that picking it would change nothing
// before they pick it.
func OSSandboxAvailable() bool { return osAvailable() }

// OSSandboxDetail describes, in one line, what the os backend enforces on this
// host — or why it cannot be used. The Linux answer depends on the running
// kernel, so this is a runtime question, not a compile-time one.
func OSSandboxDetail() string {
	if !osAvailable() {
		return osUnavailable()
	}
	return osDetail()
}

// Run executes s under p, dispatching to the selected backend. It returns an
// error only for start failures; command exit status and timeouts are reported
// in the Result.
func Run(ctx context.Context, p Policy, s Spec) (Result, error) {
	p = p.withDefaults()
	backend, degraded := selectBackend(p)
	if p.Strict {
		if err := strictBackend(p, backend, degraded); err != nil {
			return Result{ExitCode: -1}, err
		}
	}

	start := time.Now()
	var res Result
	var err error
	switch backend {
	case "docker":
		res, err = runDocker(ctx, p, s)
	case "os":
		res, err = runOS(ctx, p, s)
	default:
		res, err = runNative(ctx, p, s)
	}
	res.Backend = backend
	res.Mode = p.Mode
	res.Degraded = joinNotes(degraded, res.Degraded)
	res.Duration = time.Since(start)
	// Whatever we injected must not come back out. The output is a tool result:
	// it goes into the model's context, the session record and the console, so a
	// script that echoes a credential — deliberately, via `set -x`, or in an
	// error message — would put it in the conversation for good.
	redactions := make(map[string]string, len(s.Env)+len(s.Secrets))
	for k, v := range s.Env {
		redactions[k] = v
	}
	for k, v := range s.Secrets {
		redactions[k] = v
	}
	res.Output = redactInjected(res.Output, redactions, res.Truncated)
	res.Stdout = redactInjected(res.Stdout, redactions, res.Truncated)
	res.Stderr = redactInjected(res.Stderr, redactions, res.Truncated)
	if res.Truncated {
		res.Output += "\n…（输出已截断）"
	}

	if Audit != nil {
		ev := AuditEvent{
			Backend:   res.Backend,
			Mode:      res.Mode,
			Degraded:  res.Degraded,
			Dir:       s.Dir,
			Interp:    s.Interp,
			File:      s.RelFile,
			Args:      s.Args,
			Duration:  res.Duration,
			ExitCode:  res.ExitCode,
			TimedOut:  res.TimedOut,
			Cancelled: res.Cancelled,
		}
		if err != nil {
			ev.Err = err.Error()
		}
		Audit(ev)
	}
	return res, err
}

// CheckStrict checks the same envelope as Run without starting a process.
func CheckStrict(p Policy) error {
	p = p.withDefaults()
	backend, degraded := selectBackend(p)
	return strictBackend(p, backend, degraded)
}

func strictBackend(p Policy, backend, degraded string) error {
	if backend == "native" || degraded != "" || !p.Mode.Confines() {
		return fmt.Errorf("通用执行器要求完整沙箱，拒绝降级：%s", degraded)
	}
	if backend == "os" {
		return osStrictError(p)
	}
	return nil
}

// The generic executor must not inherit broad /etc, /proc or device reads from
// the legacy skill envelope. Keep interpreters, trust roots and resolver files.
func systemPaths(p Policy) []string {
	if !p.Strict {
		return systemReadPaths
	}
	return []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/opt", "/System", "/Library/Developer", "/Library/Apple",
		"/private/var/db/dyld", "/private/var/select", "/etc/ssl", "/etc/pki", "/etc/hosts", "/etc/resolv.conf",
		"/etc/nsswitch.conf", "/etc/passwd", "/etc/ld.so.cache", "/proc/self", "/proc/meminfo", "/proc/cpuinfo",
		"/sys/devices/system/cpu", "/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/fd", "/dev/stdout", "/dev/stderr"}
}

// minRedactLen is the shortest injected value worth masking. A value of three
// characters or fewer cannot be a credential, and masking one would wreck every
// output that happens to contain it — "1" or "ok" appears in most of them.
const minRedactLen = 4

// Redact applies the same injected-value masking to ancillary execution data.
func Redact(out string, env map[string]string, truncated bool) string {
	return redactInjected(out, env, truncated)
}

// redactInjected replaces each injected value in out with a reference to the
// variable it came from, so the reader still sees the shape of the output
// ("token=${API_TOKEN}") while the value itself never leaves the sandbox.
//
// Longest values go first: when one injected value contains another, replacing
// the short one first would leave a mangled fragment of the long one behind.
//
// This stops a value escaping by accident — an echo, a set -x, a stack trace. It
// cannot stop a script that is actively trying to exfiltrate, since anything can
// be base64-encoded or printed one character at a time. The defence against that
// one is not running scripts you have not read.
func redactInjected(out string, env map[string]string, truncated bool) string {
	if out == "" || len(env) == 0 {
		return out
	}
	names := make([]string, 0, len(env))
	for k, v := range env {
		if len(v) >= minRedactLen && !publicSetting(k) {
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return out
	}
	// Sort by value length (longest first), name as the tie-break so the result
	// does not depend on map iteration order.
	sort.Slice(names, func(i, j int) bool {
		if a, b := len(env[names[i]]), len(env[names[j]]); a != b {
			return a > b
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		out = strings.ReplaceAll(out, env[name], "${"+name+"}")
	}
	if truncated {
		// The cap can fall in the middle of a value, leaving a prefix that no
		// replacement matches. A prefix of a token is still part of the token.
		for _, name := range names {
			v := env[name]
			for k := len(v) - 1; k >= minRedactLen; k-- {
				if strings.HasSuffix(out, v[:k]) {
					out = out[:len(out)-k] + "${" + name + "}"
					break
				}
			}
		}
	}
	return out
}

// publicSetting reports whether an injected variable names a location rather
// than a secret — a region, zone or namespace. Masking those wrecked answers:
// TENCENTCLOUD_REGION=ap-shanghai turned every "ap-shanghai" in an API reply
// into ${TENCENTCLOUD_REGION}. The rule is by name and deliberately narrow:
// anything else stays masked, and a name that also says secret, key, token or
// password is masked whatever it ends with.
func publicSetting(name string) bool {
	n := strings.ToUpper(name)
	for _, secret := range []string{"SECRET", "KEY", "TOKEN", "PASS", "CREDENTIAL", "AUTH"} {
		if strings.Contains(n, secret) {
			return false
		}
	}
	for _, suffix := range []string{"REGION", "ZONE", "NAMESPACE"} {
		if strings.HasSuffix(n, suffix) {
			return true
		}
	}
	return false
}

// joinNotes concatenates the non-empty degradation notes, in order.
func joinNotes(notes ...string) string {
	var kept []string
	for _, n := range notes {
		if n != "" {
			kept = append(kept, n)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	out := kept[0]
	for _, n := range kept[1:] {
		out += "；" + n
	}
	return out
}

// capture runs cmd with combined stdout+stderr buffered and capped at max bytes.
// It returns the (possibly truncated) output, whether truncation happened, the
// process exit code, and — only when the command failed to start (not merely a
// non-zero exit) — a start error.
func capture(cmd *exec.Cmd, max int) (res Result, startErr error) {
	// Drain both pipes, bounding memory while the process runs.
	var mu sync.Mutex
	combined := &boundedOutput{max: max, mu: &mu}
	stdout := &boundedOutput{max: max, mu: &mu}
	stderr := &boundedOutput{max: max, mu: &mu}
	cmd.Stdout = io.MultiWriter(stdout, combined)
	cmd.Stderr = io.MultiWriter(stderr, combined)
	runErr := cmd.Run()
	res.Started = cmd.Process != nil
	res.Output, res.Stdout, res.Stderr = combined.buf.String(), stdout.buf.String(), stderr.buf.String()
	res.Truncated = combined.truncated || stdout.truncated || stderr.truncated
	if runErr != nil {
		if errors.Is(runErr, exec.ErrWaitDelay) {
			res.Truncated = true // inherited output pipes were forcibly closed
		}
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		res.ExitCode = -1
		return res, runErr
	}
	return res, nil
}

type boundedOutput struct {
	buf       bytes.Buffer
	max       int
	mu        *sync.Mutex
	truncated bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.max > 0 && b.buf.Len()+len(p) > b.max {
		p = p[:b.max-b.buf.Len()]
		b.truncated = true
	}
	b.buf.Write(p)
	return n, nil
}
