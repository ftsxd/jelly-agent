package sandbox

import (
	"strings"
	"testing"
)

// The Linux backend hands its policy to a second process as argv, so the two
// halves have to agree exactly. The confinement itself can only be exercised on
// Linux, but this contract can — and it is where a silent mistake would cost the
// most: a flag the helper does not understand means a target running under a
// weaker policy than the operator configured.
func TestHelperArgvRoundTrip(t *testing.T) {
	cases := []struct {
		mode      Mode
		wantWrite bool
		wantNoNet bool
	}{
		{ModeReadOnly, false, true},
		{ModeWorkspace, true, true},
		{ModeWorkspaceNet, true, false},
	}
	for _, c := range cases {
		p := Policy{Mode: c.mode, ReadPaths: []string{"/"}}
		argv := helperArgv("/usr/local/bin/jelly", p, "/work")

		if argv[0] != "/usr/local/bin/jelly" || argv[1] != HelperCommand {
			t.Fatalf("%s: argv does not re-enter the helper: %v", c.mode, argv[:2])
		}
		if argv[len(argv)-1] != "--" {
			t.Fatalf("%s: argv must end with the -- terminator: %v", c.mode, argv)
		}

		target := []string{"python3", "/work/x.py", "--flag=v"}
		spec, err := parseHelperArgs(append(argv[2:], target...))
		if err != nil {
			t.Fatalf("%s: helper cannot parse its own argv: %v", c.mode, err)
		}
		if got := strings.Join(spec.Cmd, " "); got != strings.Join(target, " ") {
			t.Errorf("%s: target mangled: %q", c.mode, got)
		}
		if c.wantWrite != containsPath(spec.ReadWrite, "/work") {
			t.Errorf("%s: workspace writable = %v, want %v", c.mode, !c.wantWrite, c.wantWrite)
		}
		if !c.wantWrite && !containsPath(spec.ReadOnly, "/work") {
			t.Errorf("%s: read-only mode must still let the script read its own directory", c.mode)
		}
		if spec.NoNet != c.wantNoNet {
			t.Errorf("%s: NoNet = %v, want %v", c.mode, spec.NoNet, c.wantNoNet)
		}
		// A target flag that looks like one of ours must not be eaten as policy.
		if containsPath(spec.ReadOnly, "v") || containsPath(spec.ReadWrite, "v") {
			t.Errorf("%s: target arguments leaked into the policy: %+v", c.mode, spec)
		}
	}
}

func TestParseHelperArgsRejectsBadInput(t *testing.T) {
	// An unknown flag must fail rather than be skipped: skipping it would run the
	// target under a weaker policy than was asked for.
	if _, err := parseHelperArgs([]string{"--ro=/usr", "--allow-everything", "--", "sh"}); err == nil {
		t.Error("unknown flag was accepted")
	}
	if _, err := parseHelperArgs([]string{"--ro=/usr", "--"}); err == nil {
		t.Error("empty command was accepted")
	}
	if _, err := parseHelperArgs([]string{"--ro=/usr"}); err == nil {
		t.Error("missing -- terminator was accepted")
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// WritePaths has to cross the process boundary as --rw, and must collapse to
// --ro under read-only for the same reason the workspace does.
func TestHelperArgvCarriesWritePaths(t *testing.T) {
	p := Policy{Mode: ModeWorkspace, WritePaths: []string{"/"}}
	spec, err := parseHelperArgs(append(helperArgv("jelly", p, "/work")[2:], "sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !containsPath(spec.ReadWrite, "/") {
		t.Errorf("write path missing from argv: %+v", spec.ReadWrite)
	}

	p.Mode = ModeReadOnly
	spec, err = parseHelperArgs(append(helperArgv("jelly", p, "/work")[2:], "sh"))
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.ReadWrite) != 0 {
		t.Errorf("read-only mode still handed out writable paths: %+v", spec.ReadWrite)
	}
	if !containsPath(spec.ReadOnly, "/") {
		t.Errorf("under read-only a write path must still be readable: %+v", spec.ReadOnly)
	}
}
