package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

// bwrapAvailable reports whether bwrap is installed AND can run a command in
// the layout real runs get. Presence alone is not enough: Docker's default
// seccomp profile, a disabled kernel.unprivileged_userns_clone, or Ubuntu's
// AppArmor userns restriction all leave the binary on PATH yet make every run
// fail at start — which would slip past the Strict preflight and surface only as
// a vague start error. The probe binds the Strict system allowlist rather than
// the whole root, so a layout that cannot even exec `true` (a missing dynamic
// loader, say) is caught here too. One run settles it for the process lifetime.
func bwrapAvailable() bool {
	bwrapOnce.Do(func() {
		p, err := exec.LookPath("bwrap")
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m := newBwrapMounts()
		m.bindAll(systemPaths(Policy{Strict: true}), "--ro-bind-try")
		args := append(append([]string{}, bwrapIsolation...), m.argv...)
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
	m := newBwrapMounts()
	m.bindAll(systemPaths(p), "--ro-bind-try")
	m.bindAll(p.ReadPaths, "--ro-bind-try")
	rw := "--ro-bind-try"
	if p.Mode.CanWrite() {
		rw = "--bind-try"
	}
	m.bindAll(append([]string{dir}, p.WritePaths...), rw)
	argv := append(append([]string{bwrapPath}, bwrapIsolation...), m.argv...)
	argv = append(argv, "--chdir", dir)
	if !p.Mode.CanNetwork() {
		// Unconditional: bwrap simply does not give the sandboxed process a
		// network namespace, so there is no ABI/kernel-version gate to miss the
		// way there is with Landlock's UDP/QUIC gap.
		argv = append(argv, "--unshare-net")
	}
	return append(argv, "--")
}

// bwrapMounts accumulates bind and symlink operations; each symlink is
// created once.
type bwrapMounts struct {
	argv  []string
	seen  map[string]bool
	binds []string
}

func newBwrapMounts() *bwrapMounts { return &bwrapMounts{seen: map[string]bool{}} }

func (m *bwrapMounts) bindAll(paths []string, flag string) {
	for _, r := range paths {
		m.bind(r, flag)
	}
}

// bind exposes path inside the sandbox at the same place it has outside.
//
// Binding only the resolved target is not enough: on merged-/usr systems
// (Debian 12, the python:slim image, RHEL 7+) /bin, /lib and /lib64 are
// symlinks into /usr, and the ELF interpreter every binary names is
// /lib64/ld-linux-x86-64.so.2. Without the symlinks themselves the sandbox has
// /usr/lib but no /lib64, so every exec fails with ENOENT — reported by bwrap
// as "execvp sh: No such file or directory", which reads like a missing
// command. So each symlink hop is recreated with --symlink as well.
func (m *bwrapMounts) bind(path, flag string) {
	c := canonical(path)
	if c == "" || bwrapSynthetic(path) || bwrapSynthetic(c) {
		return
	}
	for hop, n := path, 0; n < 8; n++ {
		fi, err := os.Lstat(hop)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			break
		}
		target, err := os.Readlink(hop)
		if err != nil {
			break
		}
		if !m.seen[hop] && !m.underBind(hop) {
			m.seen[hop] = true
			m.argv = append(m.argv, "--symlink", target, hop)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(hop), target)
		}
		hop = target
	}
	if !m.seen[c] {
		m.seen[c] = true
		m.binds = append(m.binds, c)
	}
	// Always emitted, even for a path bound before: a write path that is also a
	// read path must end up read-write, and the later bind is the one that wins.
	m.argv = append(m.argv, flag, c, c)
}

// underBind reports whether path sits inside a directory already bound: the
// symlink is then in the sandbox as-is, and creating it again would fail on
// the read-only mount.
func (m *bwrapMounts) underBind(path string) bool {
	for _, b := range m.binds {
		if strings.HasPrefix(path, strings.TrimSuffix(b, "/")+"/") {
			return true
		}
	}
	return false
}

// bwrapSynthetic reports whether path lies under /proc or /dev, which bwrap
// synthesises fresh for the sandbox. Binding anything from the real ones back
// in would defeat that — and worse than it looks: canonical("/proc/self") is
// the agent's own /proc/<pid>, whose environ holds the service's secrets.
func bwrapSynthetic(path string) bool {
	for _, root := range []string{"/proc", "/dev"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

// runBwrap runs the script inside a bwrap sandbox.
func runBwrap(ctx context.Context, p Policy, s Spec) (Result, error) {
	dir := canonical(s.Dir)
	return runLocal(ctx, p, s, bwrapArgv(p, dir))
}
