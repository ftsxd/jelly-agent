package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestModeDefaultsAndLegacyNetworkFlag(t *testing.T) {
	if got := (Policy{}).withDefaults().Mode; got != ModeWorkspace {
		t.Fatalf("zero policy mode = %q, want %q", got, ModeWorkspace)
	}
	// Config written before modes existed only had the Network boolean; it must
	// keep meaning exactly what it used to.
	if got := (Policy{Network: true}).withDefaults().Mode; got != ModeWorkspaceNet {
		t.Fatalf("legacy network=true mode = %q, want %q", got, ModeWorkspaceNet)
	}
	if got := (Policy{Mode: "nonsense"}).withDefaults().Mode; got != DefaultMode {
		t.Fatalf("invalid mode = %q, want fallback to %q", got, DefaultMode)
	}
	// An explicit mode wins over the legacy boolean.
	if got := (Policy{Mode: ModeReadOnly, Network: true}).withDefaults().Mode; got != ModeReadOnly {
		t.Fatalf("explicit mode lost to legacy flag: %q", got)
	}
}

// A per-skill mode may only tighten the operator's envelope, never loosen it.
func TestModeAtMostClamps(t *testing.T) {
	cases := []struct{ skill, global, want Mode }{
		{ModeReadOnly, ModeWorkspaceNet, ModeReadOnly},   // skill tightens: honored
		{ModeWorkspaceNet, ModeWorkspace, ModeWorkspace}, // skill wants net, operator said no
		{ModeFull, ModeReadOnly, ModeReadOnly},           // skill wants everything: denied
		{ModeWorkspace, ModeWorkspace, ModeWorkspace},
	}
	for _, c := range cases {
		if got := c.skill.AtMost(c.global); got != c.want {
			t.Errorf("%q.AtMost(%q) = %q, want %q", c.skill, c.global, got, c.want)
		}
	}
}

func TestModeCapabilities(t *testing.T) {
	if ModeReadOnly.CanWrite() {
		t.Error("read-only must not permit writes")
	}
	if ModeWorkspace.CanNetwork() {
		t.Error("workspace must not permit the network")
	}
	if !ModeWorkspaceNet.CanNetwork() || !ModeWorkspaceNet.CanWrite() {
		t.Error("workspace-net must permit both")
	}
	if ModeFull.Confines() {
		t.Error("full must not claim to confine anything")
	}
}

// mode=full asks for no isolation, so it must run native and say out loud that
// nothing was enforced — a run that looks sandboxed but is not is the one
// outcome worse than no sandbox.
func TestFullModeRunsUnconfinedAndSaysSo(t *testing.T) {
	dir, name := writeScript(t, "ok.sh", "#!/bin/sh\necho ok")
	res, err := Run(context.Background(), Policy{Mode: ModeFull}, Spec{Dir: dir, Interp: "sh", RelFile: name})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Backend != "native" {
		t.Fatalf("backend = %q, want native", res.Backend)
	}
	if res.Degraded == "" {
		t.Fatal("an unconfined run must be reported as such")
	}
}

// The mode has to mean the same thing on every backend. It would be a nasty
// surprise if switching to the strongest backend silently closed the network on
// a policy that grants it — or left it open on one that does not.
func TestDockerArgsFollowTheMode(t *testing.T) {
	cases := []struct {
		mode        Mode
		wantNoNet   bool
		wantROMount bool
	}{
		{ModeReadOnly, true, true},
		{ModeWorkspace, true, false},
		{ModeWorkspaceNet, false, false},
	}
	for _, c := range cases {
		args := dockerArgs(Policy{Mode: c.mode}.withDefaults(), Spec{Dir: "/w"})
		joined := strings.Join(args, " ")
		if got := strings.Contains(joined, "--network none"); got != c.wantNoNet {
			t.Errorf("%s: --network none = %v, want %v", c.mode, got, c.wantNoNet)
		}
		if got := strings.Contains(joined, "/w:/work:ro"); got != c.wantROMount {
			t.Errorf("%s: read-only mount = %v, want %v", c.mode, got, c.wantROMount)
		}
	}
}
