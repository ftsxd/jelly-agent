package sandbox

import (
	"errors"
	"fmt"
	"strings"
)

// The argv contract between the "os" backend's Linux half and the hidden
// HelperCommand subcommand. Both halves live here, platform-independent and
// therefore testable everywhere — the Linux confinement itself can only be
// exercised on Linux, but the wire format between the two processes need not be.

const (
	flagReadOnly  = "--ro="
	flagReadWrite = "--rw="
	flagNoNet     = "--no-net"
)

// helperSpec is the parsed form of the helper's argv.
type helperSpec struct {
	ReadOnly  []string // paths the target may read
	ReadWrite []string // paths the target may read and write
	NoNet     bool     // deny network access
	Cmd       []string // the target command, argv[0] first
}

// helperArgv renders the argv that re-executes exe as the sandbox helper for one
// run. The target command is appended by the caller after the "--" terminator.
func helperArgv(exe string, p Policy, dir string) []string {
	argv := []string{exe, HelperCommand}
	for _, r := range systemReadPaths {
		argv = append(argv, flagReadOnly+r)
	}
	for _, r := range p.ReadPaths {
		if c := canonical(r); c != "" {
			argv = append(argv, flagReadOnly+c)
		}
	}
	// The workspace is the one path whose access depends on the mode: writable
	// normally, readable only under read-only. The operator's extra write paths
	// follow the same rule — under read-only nothing is writable, including
	// them, or the mode would not mean what it says.
	writable := []string{dir}
	writable = append(writable, p.WritePaths...)
	for _, w := range writable {
		c := canonical(w)
		if c == "" {
			continue
		}
		if p.Mode.CanWrite() {
			argv = append(argv, flagReadWrite+c)
		} else {
			argv = append(argv, flagReadOnly+c)
		}
	}
	if !p.Mode.CanNetwork() {
		argv = append(argv, flagNoNet)
	}
	return append(argv, "--")
}

// parseHelperArgs reads the argv helperArgv produced (everything after the
// subcommand name). An unrecognized flag is an error rather than something to
// skip: silently ignoring a restriction we failed to understand would run the
// target with a weaker policy than the caller asked for.
func parseHelperArgs(args []string) (helperSpec, error) {
	var spec helperSpec
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			spec.Cmd = args[i+1:]
			i = len(args)
		case strings.HasPrefix(a, flagReadOnly):
			spec.ReadOnly = append(spec.ReadOnly, strings.TrimPrefix(a, flagReadOnly))
		case strings.HasPrefix(a, flagReadWrite):
			spec.ReadWrite = append(spec.ReadWrite, strings.TrimPrefix(a, flagReadWrite))
		case a == flagNoNet:
			spec.NoNet = true
		default:
			return spec, fmt.Errorf("sandbox helper: 无法识别的参数 %q", a)
		}
	}
	if len(spec.Cmd) == 0 {
		return spec, errors.New("sandbox helper: -- 之后没有要执行的命令")
	}
	return spec, nil
}
