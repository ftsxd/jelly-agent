package sandbox

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// The other half of the Linux "os" backend: bubblewrap (bwrap), the same
// unprivileged mount/user/PID-namespace primitive Codex CLI and Claude Code use
// as their default Linux sandbox. Unlike Landlock it needs no particular kernel
// version and no boot parameter — only the bwrap binary and (typically enabled
// by default) unprivileged user namespaces — so it is what keeps the generic
// executor working on hosts that will never have `lsm=landlock` set.

var (
	bwrapOnce sync.Once
	bwrapPath string
	bwrapErr  string // why an installed bwrap still cannot be used; "" if not installed
)

// bwrapIsolation is the namespace/mount envelope every run gets. The probe
// below tries exactly this, so "available" means these flags actually work.
var bwrapIsolation = []string{
	"--unshare-user-try", // some distros still gate userns; degrade, don't fail
	"--unshare-pid",
	"--unshare-ipc",
	"--unshare-uts",
	"--unshare-cgroup-try",
	"--die-with-parent",
	"--proc", "/proc",
	"--dev", "/dev",
	"--tmpfs", "/tmp",
}

// bwrapAvailable reports whether bwrap is installed AND can create its
// namespaces here. Presence alone is not enough: Docker's default seccomp
// profile, a disabled kernel.unprivileged_userns_clone, or Ubuntu's AppArmor
// userns restriction all leave the binary on PATH yet make every run fail at
// start — which would slip past the Strict preflight and surface only as a
// vague start error. One real no-op run settles it for the process lifetime.
func bwrapAvailable() bool {
	bwrapOnce.Do(func() {
		p, err := exec.LookPath("bwrap")
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		args := append([]string{"--ro-bind", "/", "/"}, bwrapIsolation...)
		args = append(args, "--unshare-net", "--", "true")
		out, err := exec.CommandContext(ctx, p, args...).CombinedOutput()
		if err != nil {
			bwrapErr, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n")
			if bwrapErr == "" {
				bwrapErr = err.Error()
			}
			return
		}
		bwrapPath = p
	})
	return bwrapPath != ""
}

// bwrapUnavailable says, for the operator, why bwrap cannot be used here.
func bwrapUnavailable() string {
	bwrapAvailable()
	if bwrapErr == "" {
		return "未安装 bubblewrap"
	}
	return "bubblewrap 已安装但无法创建沙箱（容器内需放宽 seccomp/AppArmor/systempaths，或主机禁用了非特权 user namespace）：" + bwrapErr
}

// bwrapArgv renders the `bwrap …` prefix for one run: new user/PID/IPC/UTS
// namespaces, a synthetic /proc and /dev, the read allowlist bound read-only,
// the workspace (and any extra write paths) bound read-write unless the mode is
// read-only, and the network namespace dropped entirely unless the mode allows
// it. The caller appends the target command after the returned "--".
func bwrapArgv(p Policy, dir string) []string {
	argv := append([]string{bwrapPath}, bwrapIsolation...)
	for _, r := range systemPaths(p) {
		// /proc and /dev are already synthesised above; a later bind of the real
		// ones would overlay them and defeat the PID namespace.
		if r == "/proc" || r == "/dev" {
			continue
		}
		if c := canonical(r); c != "" {
			argv = append(argv, "--ro-bind-try", c, c)
		}
	}
	for _, r := range p.ReadPaths {
		if c := canonical(r); c != "" {
			argv = append(argv, "--ro-bind-try", c, c)
		}
	}
	writable := append([]string{dir}, p.WritePaths...)
	for _, w := range writable {
		c := canonical(w)
		if c == "" {
			continue
		}
		if p.Mode.CanWrite() {
			argv = append(argv, "--bind-try", c, c)
		} else {
			argv = append(argv, "--ro-bind-try", c, c)
		}
	}
	argv = append(argv, "--chdir", dir)
	if !p.Mode.CanNetwork() {
		// Unconditional: bwrap simply does not give the sandboxed process a
		// network namespace, so there is no ABI/kernel-version gate to miss the
		// way there is with Landlock's UDP/QUIC gap.
		argv = append(argv, "--unshare-net")
	}
	return append(argv, "--")
}

// runBwrap runs the script inside a bwrap sandbox.
func runBwrap(ctx context.Context, p Policy, s Spec) (Result, error) {
	dir := canonical(s.Dir)
	return runLocal(ctx, p, s, bwrapArgv(p, dir))
}
