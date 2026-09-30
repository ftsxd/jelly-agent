package sandbox

import (
	"context"
	"os/exec"
	"strings"
	"sync"
)

// The macOS half of the "os" backend: Seatbelt, driven by /usr/bin/sandbox-exec.
// The same primitive Codex CLI and Qwen Code use on macOS — it ships with the
// system, needs no daemon and no privileges, and costs one exec.

var (
	seatbeltOnce sync.Once
	seatbeltPath string
)

func seatbelt() string {
	seatbeltOnce.Do(func() {
		if p, err := exec.LookPath("sandbox-exec"); err == nil {
			seatbeltPath = p
		}
	})
	return seatbeltPath
}

func osAvailable() bool { return seatbelt() != "" }

func osStrictError(Policy) error { return nil }

func osUnavailable() string {
	if osAvailable() {
		return ""
	}
	return "本机没有 sandbox-exec"
}

// osDetail describes what Seatbelt gives us here. macOS is the uniform case:
// the profile either applies in full or sandbox-exec refuses to start the child.
func osDetail() string {
	return "macOS Seatbelt（sandbox-exec）：读限系统目录+工作目录，写限工作目录，可断网"
}

// systemReadPaths are the host locations a script may read regardless of mode:
// the interpreters themselves, their standard libraries, and what dyld needs to
// load anything at all. Everything outside this list plus the workspace is
// unreadable — which is the point, since the agent's own config and state (API
// keys, sessions) live outside it.
var systemReadPaths = []string{
	"/usr", "/bin", "/sbin", "/opt", "/System", "/Library",
	"/private/var/db/dyld", "/private/var/select", "/dev", "/private/etc",
}

// runOS runs the script behind a generated Seatbelt profile.
func runOS(ctx context.Context, p Policy, s Spec) (Result, error) {
	dir := canonical(s.Dir)
	prof := seatbeltProfile(p, dir)
	return runLocal(ctx, p, s, []string{seatbelt(), "-p", prof})
}

// seatbeltProfile builds the SBPL policy for one run. It is deny-by-default:
// only the listed reads, the workspace writes, and (when the mode allows it) the
// network get through. The rules are ordered loosest to tightest because SBPL is
// last-match-wins.
func seatbeltProfile(p Policy, dir string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")

	// Running anything at all: exec/fork, signalling our own children, reading
	// sysctls and looking up the system services libSystem needs on startup.
	b.WriteString(`
(allow process-exec*)
(allow process-fork)
(allow signal (target same-sandbox))
(allow sysctl-read)
(allow ipc-posix-shm)
(allow file-read-metadata)
(allow file-ioctl (subpath "/dev"))
`)

	// Mach services by name, never all of them: an open mach-lookup lets a
	// child reach launch services, the pasteboard or another app, all of which
	// run outside the sandbox. This is what libSystem, logging, user/group
	// lookup, preferences and certificate checks need — the same base set
	// Codex CLI allows.
	b.WriteString("(allow mach-lookup " + globalNames(baseMachServices) + ")\n")

	// Reads: the system allowlist, the workspace, and whatever the operator
	// added. Metadata (stat) is already allowed globally above — too much
	// breaks without it, and it discloses no content.
	reads := append(append([]string{}, systemPaths(p)...), dir)
	for i, r := range reads {
		reads[i] = canonical(r)
	}
	for _, extra := range p.ReadPaths {
		if c := canonical(extra); c != "" {
			reads = append(reads, c)
		}
	}
	// (literal "/") is not decoration: without read access to the root directory
	// itself, dyld cannot resolve anything and every child dies with SIGABRT
	// before it runs a single instruction. Verified the hard way.
	b.WriteString(`(allow file-read* (literal "/") ` + subpaths(reads) + ")\n")

	// Writes: the workspace and whatever the operator listed, plus the handful
	// of character devices that everything expects to be writable. Never a whole
	// /dev subpath — that would hand over the raw disks.
	if p.Mode.CanWrite() {
		writes := []string{dir}
		for _, extra := range p.WritePaths {
			if c := canonical(extra); c != "" {
				writes = append(writes, c)
			}
		}
		b.WriteString("(allow file-write* " + subpaths(writes) + ")\n")
		// A path you may write but not read is a trap: git cannot update a
		// checkout it cannot stat, and the failure looks nothing like a policy.
		b.WriteString("(allow file-read* " + subpaths(writes) + ")\n")
	}
	b.WriteString(`(allow file-write-data
  (literal "/dev/null") (literal "/dev/zero")
  (literal "/dev/random") (literal "/dev/urandom")
  (literal "/dev/stdout") (literal "/dev/stderr") (literal "/dev/tty"))
`)

	// macOS's /usr/bin/python3 is an xcrun shim that insists on refreshing a
	// cache file in the per-user temp dir. Denying it costs two lines of
	// "Operation not permitted" on stderr in front of every script's real
	// output, which is how a sandbox ends up switched off. Allow that one file
	// name and nothing else in that directory.
	b.WriteString("(allow file-read* file-write* " + xcrunCacheRegex + ")\n")

	if p.Mode.CanNetwork() {
		b.WriteString("(allow network*)\n")
		// DNS configuration, the network daemon and revocation checks.
		b.WriteString("(allow mach-lookup " + globalNames(networkMachServices) + ")\n")
	}
	return b.String()
}

var baseMachServices = []string{
	"com.apple.bsd.dirhelper", "com.apple.cfprefsd.agent", "com.apple.cfprefsd.daemon",
	"com.apple.diagnosticd", "com.apple.logd", "com.apple.logd.events", "com.apple.secinitd",
	"com.apple.system.DirectoryService.libinfo_v1", "com.apple.system.logger",
	"com.apple.system.notification_center", "com.apple.system.opendirectoryd.libinfo",
	"com.apple.system.opendirectoryd.membership", "com.apple.trustd", "com.apple.trustd.agent",
	"com.apple.xpc.activity.unmanaged",
}

var networkMachServices = []string{
	"com.apple.SecurityServer", "com.apple.networkd", "com.apple.ocspd",
	"com.apple.SystemConfiguration.DNSConfiguration", "com.apple.SystemConfiguration.configd",
	"com.apple.dnssd.service",
}

// globalNames renders mach service names as SBPL (global-name "…") filters.
func globalNames(names []string) string {
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(`(global-name "` + sbplEscape(n) + `")`)
	}
	return b.String()
}

// xcrunCacheRegex matches only the xcrun cache file under the per-user temp
// directory — not the directory, and not its other contents.
const xcrunCacheRegex = `(regex #"^/private/var/folders/[^/]+/[^/]+/T/xcrun_db")`

// subpaths renders paths as a sequence of SBPL (subpath "…") filters.
func subpaths(paths []string) string {
	var b strings.Builder
	for i, p := range paths {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(`(subpath "` + sbplEscape(p) + `")`)
	}
	return b.String()
}

// sbplEscape quotes a path for an SBPL string literal. A path containing a quote
// or a backslash is legal on macOS, and an unescaped one would end the literal
// early and change the meaning of the policy.
func sbplEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}
