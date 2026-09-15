package sandbox

import (
	"context"
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

func osAvailable() bool {
	probe()
	return abiVer >= 1 && selfErr == nil
}

func osUnavailable() string {
	probe()
	switch {
	case abiVer < 1:
		return "内核不支持 Landlock，需要 5.13+ 且在启动时启用（lsm= 里带 landlock）"
	case selfErr != nil:
		return "定位不到自身可执行文件：" + selfErr.Error()
	}
	return ""
}

// osDetail describes what this kernel's Landlock actually gives us. The answer
// varies by kernel and is worth showing: Debian 12 (6.1) confines the filesystem
// but cannot close the network at all.
func osDetail() string {
	probe()
	d := "Linux Landlock ABI=v" + strconv.Itoa(abiVer) + "：读限系统目录+工作目录，写限工作目录"
	if abiVer >= landlockNetABI {
		return d + "，可断 TCP 出网（UDP/QUIC 不在 Landlock 管辖内）"
	}
	return d + "；网络无法限制，需要内核 6.7+（Landlock v" + strconv.Itoa(landlockNetABI) + "）"
}

// systemReadPaths are the host locations a script may read regardless of mode:
// the interpreters, their standard libraries, the loader, and the /proc entries
// every runtime consults. Everything outside this list plus the workspace is
// unreadable — which is the point, since the agent's own config and state (API
// keys, sessions, the SQLite file) live outside it.
var systemReadPaths = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/opt", "/proc", "/sys", "/dev",
}

// runOS runs the script behind the Landlock helper.
func runOS(ctx context.Context, p Policy, s Spec) (Result, error) {
	probe()
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
