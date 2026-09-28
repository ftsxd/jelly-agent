package sandbox

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"

	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// The Linux half of the "os" backend: Landlock. Go cannot run code between fork
// and exec, so the lockdown happens the way Codex CLI does it — re-exec this
// same binary with a hidden subcommand (HelperCommand) that restricts itself and
// then execs the real target. One extra exec, no extra binary to ship, no
// privileges, and nothing to install.

// HelperCommand is the hidden subcommand that applies the confinement to itself
// and execs the target. It is not meant to be typed by a human.
const HelperCommand = "__sandbox"

// landlockNetABI is the Landlock ABI version that first restricts the network
// (Linux 6.7). Below it, Landlock confines the filesystem only.
const landlockNetABI = 4

var (
	probeOnce sync.Once
	abiVer    int
	selfExe   string
	selfErr   error
)

func probe() {
	probeOnce.Do(func() {
		if v, err := ll.LandlockGetABIVersion(); err == nil {
			abiVer = v
		}
		selfExe, selfErr = os.Executable()
	})
}

// osAvailable reports whether the "os" backend has anything to run on: either
// Landlock (for the re-exec helper below) or bwrap (bwrap_linux.go).
func osAvailable() bool {
	probe()
	return (abiVer >= 1 && selfErr == nil) || bwrapAvailable()
}

// landlockSatisfiesStrict reports whether Landlock alone gives Strict mode
// everything it asks for: a real write limit (ABI v3+), and a mode that is
// allowed onto the network anyway. Landlock can never block UDP/QUIC at any
// ABI, so an offline request is never satisfied by Landlock alone — it goes to
// bwrap, or fails here rather than silently leaving traffic open.
func landlockSatisfiesStrict(p Policy) bool {
	return abiVer >= 3 && p.Mode.CanNetwork()
}

// Landlock cannot block UDP/QUIC and its write limit only firms up at ABI v3+.
// bwrap has neither gap (real namespaces, unconditional --unshare-net), so it
// satisfies Strict here even on a kernel where Landlock alone would not.
func osStrictError(p Policy) error {
	probe()
	if landlockSatisfiesStrict(p) || bwrapAvailable() {
		return nil
	}
	if abiVer < 3 {
		return fmt.Errorf("通用执行器需要 Landlock ABI v3+ 的完整文件写限制，或可用的 bubblewrap（%s），或使用 docker 后端", bwrapUnavailable())
	}
	return fmt.Errorf("Landlock 无法完全禁止 UDP/QUIC，请提供可用的 bubblewrap（%s）或为离线执行使用 docker 后端", bwrapUnavailable())
}

func osUnavailable() string {
	probe()
	if abiVer >= 1 && selfErr != nil {
		return "定位不到自身可执行文件：" + selfErr.Error()
	}
	if abiVer < 1 && !bwrapAvailable() {
		return "内核不支持 Landlock（需要 5.13+ 且启动时启用 lsm=landlock），且" + bwrapUnavailable()
	}
	return ""
}

// osDetail describes what this host's "os" backend actually gives us. The
// answer varies by mechanism and kernel and is worth showing: Debian 12 (6.1)
// confines the filesystem via Landlock but cannot close the network at all,
// while bwrap closes it unconditionally regardless of kernel version.
func osDetail() string {
	probe()
	if abiVer >= 1 && selfErr == nil {
		d := "Linux Landlock ABI=v" + strconv.Itoa(abiVer) + "：读限系统目录+工作目录，写限工作目录"
		if abiVer >= landlockNetABI {
			return d + "，可断 TCP 出网（UDP/QUIC 不在 Landlock 管辖内）"
		}
		return d + "；网络无法限制，需要内核 6.7+（Landlock v" + strconv.Itoa(landlockNetABI) + "）"
	}
	return "Linux bubblewrap：独立 mount/user/PID 命名空间，读限系统目录+工作目录，写限工作目录，可无条件断网（--unshare-net）"
}

// systemReadPaths are the host locations a script may read regardless of mode:
// the interpreters, their standard libraries, the loader, and the /proc entries
// every runtime consults. Everything outside this list plus the workspace is
// unreadable — which is the point, since the agent's own config and state (API
// keys, sessions, the SQLite file) live outside it.
var systemReadPaths = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/opt", "/proc", "/sys", "/dev",
}

// runOS runs the script through whichever mechanism actually satisfies this
// policy: Landlock's re-exec helper when it alone is enough (the cheaper path,
// no extra namespaces), otherwise bwrap when Landlock falls short of Strict or
// isn't present at all. Both were already confirmed available by osAvailable
// and, under Strict, checked against this exact policy by osStrictError before
// Run() ever calls in here.
func runOS(ctx context.Context, p Policy, s Spec) (Result, error) {
	probe()
	landlockOK := abiVer >= 1 && selfErr == nil
	if landlockOK && (!p.Strict || landlockSatisfiesStrict(p)) {
		dir := canonical(s.Dir)
		argv := helperArgv(selfExe, p, dir)
		res, err := runLocal(ctx, p, s, argv)
		// Say so when the kernel could not give us everything the mode asked for.
		// Debian 12 still ships 6.1 (ABI v2), so this is the common case, not an
		// exotic one, and a silent half-enforced policy would be worse than none.
		if !p.Mode.CanNetwork() && abiVer < landlockNetABI {
			res.Degraded = "内核 Landlock ABI=v" + strconv.Itoa(abiVer) +
				"，网络限制需要 v" + strconv.Itoa(landlockNetABI) + "（内核 6.7+）：文件系统已受限，出网未受限"
		}
		return res, err
	}
	if bwrapAvailable() {
		return runBwrap(ctx, p, s)
	}
	// Neither mechanism is enough for what was asked; this should already have
	// been refused by osStrictError under Strict. Fall back to Landlock
	// best-effort rather than running fully unconfined.
	dir := canonical(s.Dir)
	argv := helperArgv(selfExe, p, dir)
	return runLocal(ctx, p, s, argv)
}
