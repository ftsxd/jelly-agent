package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

// writeDirSkill creates a directory-form skill with a SKILL.md and the given
// extra files (name→content), marking .sh files executable.
func writeDirSkill(t *testing.T, st *Store, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(st.Dir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: "+name+"\ndescription: t\nenabled: true\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	for n, c := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(n, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunScriptInjectsEnv(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	// A comparison, not an echo: the sandbox masks injected values out of the
	// output, so echoing $WHO would test the masking instead of the injection.
	// That masking has its own tests below.
	writeDirSkill(t, st, "greeter", map[string]string{
		"run.sh": "#!/bin/sh\n[ \"$WHO\" = \"jelly\" ] && echo got-it || echo missing",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := st.RunScript(ctx, "greeter", "run.sh", nil, map[string]string{"WHO": "jelly"}, sandbox.Policy{}, Allowlist{})
	if err != nil {
		t.Fatalf("run: %v (out=%q)", err, out)
	}
	if !strings.Contains(out, "got-it") {
		t.Fatalf("env not injected, out=%q", out)
	}

	// Scripts() surfaces the runnable file.
	if got := st.Scripts("greeter"); len(got) != 1 || got[0] != "run.sh" {
		t.Fatalf("Scripts = %v", got)
	}
}

func TestRunScriptRejectsTraversal(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	writeDirSkill(t, st, "s", nil)
	ctx := context.Background()
	if _, err := st.RunScript(ctx, "s", "../../etc/passwd", nil, nil, sandbox.Policy{}, Allowlist{}); err == nil {
		t.Fatal("expected path-traversal rejection")
	}
	if _, err := st.RunScript(ctx, "s", "nope.sh", nil, nil, sandbox.Policy{}, Allowlist{}); err == nil {
		t.Fatal("expected missing-script error")
	}
}

func TestRunScriptTimeout(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	writeDirSkill(t, st, "slow", map[string]string{"loop.sh": "#!/bin/sh\nsleep 5"})
	// Exercise the policy-level timeout (not just the ctx deadline).
	if _, err := st.RunScript(context.Background(), "slow", "loop.sh", nil, nil,
		sandbox.Policy{Timeout: 200 * time.Millisecond}, Allowlist{}); err == nil {
		t.Fatal("expected timeout error")
	}
}

// writeDirSkillWithMode is writeDirSkill plus a `sandbox:` frontmatter line.
func writeDirSkillWithMode(t *testing.T, st *Store, name, mode string, files map[string]string) {
	t.Helper()
	writeDirSkill(t, st, name, files)
	fm := "---\nname: " + name + "\ndescription: t\nenabled: true\nsandbox: " + mode + "\n---\nbody"
	if err := os.WriteFile(filepath.Join(st.Dir(), name, "SKILL.md"), []byte(fm), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A skill may tighten its own envelope but never widen it. Widening would make
// the operator's policy advisory, and a skill is content — it can arrive in a
// zip or be written by the model.
func TestSkillModeOnlyTightens(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	writeDirSkillWithMode(t, st, "strict", "read-only", nil)
	writeDirSkillWithMode(t, st, "greedy", "full", nil)
	writeDirSkillWithMode(t, st, "broken", "not-a-mode", nil)
	writeDirSkill(t, st, "silent", nil)

	global := sandbox.Policy{Mode: sandbox.ModeWorkspace}
	cases := []struct {
		skill string
		want  sandbox.Mode
	}{
		{"strict", sandbox.ModeReadOnly},  // tighter: honored
		{"greedy", sandbox.ModeWorkspace}, // wider: clamped back
		{"broken", sandbox.ModeWorkspace}, // malformed: fall back, do not fail open
		{"silent", sandbox.ModeWorkspace}, // undeclared: inherit
		{"missing", sandbox.ModeWorkspace},
	}
	for _, c := range cases {
		if got := st.SkillMode(c.skill, global); got != c.want {
			t.Errorf("SkillMode(%q) = %q, want %q", c.skill, got, c.want)
		}
	}

	// An empty global policy means "the default", not "no confinement".
	if got := st.SkillMode("greedy", sandbox.Policy{}); got != sandbox.DefaultMode {
		t.Errorf("with an unset policy, SkillMode = %q, want %q", got, sandbox.DefaultMode)
	}
}

// Saving a skill must preserve its sandbox declaration. The web form rewrites
// the whole file on every edit — including a mere enable/disable toggle — so a
// dropped field here would quietly widen the envelope of an already-deployed
// skill.
func TestSaveRoundTripsSandboxMode(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	if err := st.Save(Skill{Name: "probe", Description: "d", Enabled: true, Sandbox: "read-only", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	sk, ok, err := st.Get("probe")
	if err != nil || !ok {
		t.Fatalf("get: %v %v", ok, err)
	}
	if sk.Sandbox != "read-only" {
		t.Fatalf("sandbox declaration lost on save: %q", sk.Sandbox)
	}

	if err := st.Save(Skill{Name: "bad", Description: "d", Enabled: true, Sandbox: "wide-open"}); err == nil {
		t.Fatal("an invalid sandbox mode was accepted")
	}
}

// End to end: the declaration actually narrows what the script can do.
func TestRunScriptHonorsSkillMode(t *testing.T) {
	if !sandbox.OSSandboxAvailable() {
		t.Skipf("os 后端不可用：%s", sandbox.OSSandboxDetail())
	}
	st, _ := NewStore(t.TempDir())
	writeDirSkillWithMode(t, st, "reader", "read-only", map[string]string{
		"run.sh": "#!/bin/sh\necho x > ./out.txt && echo WROTE\nexit 0",
	})

	// The operator's policy would allow the write; the skill's own does not.
	out, err := st.RunScript(context.Background(), "reader", "run.sh", nil, nil,
		sandbox.Policy{Backend: "os", Mode: sandbox.ModeWorkspaceNet}, Allowlist{})
	if err != nil {
		t.Fatalf("run: %v (%s)", err, out)
	}
	if strings.Contains(out, "WROTE") {
		t.Fatalf("skill declared read-only but wrote anyway: %q", out)
	}
}

// The allowlist has to bite at the point of execution, not only in the catalog.
// A model that has seen a skill name once — in an earlier turn, in a handover,
// in its own transcript — can name it again; "it was never shown the name" is
// not an access control.
func TestRunScriptRefusesASkillOutsideTheAllowlist(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	writeDirSkill(t, st, "secret", map[string]string{"run.sh": "#!/bin/sh\necho RAN"})

	out, err := st.RunScript(context.Background(), "secret", "run.sh", nil, nil,
		sandbox.Policy{}, NewAllowlist(&[]string{"something-else"}))
	if err == nil {
		t.Fatalf("a skill outside the allowlist executed: %q", out)
	}
	if strings.Contains(out, "RAN") {
		t.Fatalf("the script ran despite the refusal: %q", out)
	}
	// An agent given no skills at all must be refused the same way.
	if _, err := st.RunScript(context.Background(), "secret", "run.sh", nil, nil,
		sandbox.Policy{}, NewAllowlist(&[]string{})); err == nil {
		t.Fatal("an agent with no skills ran a script")
	}
	// ...and the unrestricted default still works.
	if _, err := st.RunScript(context.Background(), "secret", "run.sh", nil, nil,
		sandbox.Policy{}, Allowlist{}); err != nil {
		t.Fatalf("unrestricted run failed: %v", err)
	}
}

// use_skill resolves through Visible, so the same three reasons collapse there.
func TestVisibleHidesWhatTheAgentMayNotUse(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	if err := st.Save(Skill{Name: "shown", Description: "d", Enabled: true, Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(Skill{Name: "disabled", Description: "d", Enabled: false, Body: "b"}); err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := st.Visible("shown", Allowlist{}); !ok {
		t.Error("an enabled skill must be visible when unrestricted")
	}
	if _, ok, _ := st.Visible("disabled", Allowlist{}); ok {
		t.Error("a disabled skill must not be visible")
	}
	if _, ok, _ := st.Visible("shown", NewAllowlist(&[]string{"other"})); ok {
		t.Error("a skill outside the allowlist must not be visible")
	}
	if _, ok, _ := st.Visible("missing", Allowlist{}); ok {
		t.Error("a missing skill must not be visible")
	}
}

// The output of a script is a tool result: it reaches the model, the session
// record and the console. A credential must not survive that trip, whether the
// script prints it on purpose, dumps the whole environment, or leaks it through
// a trace or an error message.
func TestRunScriptOutputNeverCarriesTheInjectedValue(t *testing.T) {
	const secret = "sk-super-secret-value-123"
	st, _ := NewStore(t.TempDir())
	writeDirSkill(t, st, "probe", map[string]string{
		"echo.sh":  "#!/bin/sh\necho \"token=$API_TOKEN\"",
		"dump.sh":  "#!/bin/sh\nenv",
		"trace.sh": "#!/bin/sh\nset -x\ncurl_stub \"$API_TOKEN\" 2>/dev/null\ntrue",
		"fail.sh":  "#!/bin/sh\necho \"cannot authenticate with $API_TOKEN\" >&2\nexit 7",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, script := range []string{"echo.sh", "dump.sh", "trace.sh", "fail.sh"} {
		out, _ := st.RunScript(ctx, "probe", script, nil,
			map[string]string{"API_TOKEN": secret}, sandbox.Policy{}, Allowlist{})
		if strings.Contains(out, secret) {
			t.Errorf("%s leaked the value into its output: %q", script, out)
		}
		if !strings.Contains(out, "${API_TOKEN}") {
			t.Errorf("%s: want the masked reference in the output, got %q", script, out)
		}
	}
}

// Masking must not cost the script its own output.
func TestRunScriptOutputKeepsEverythingElse(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	writeDirSkill(t, st, "report", map[string]string{
		"run.sh": "#!/bin/sh\necho \"3 pods ready in $CLUSTER, token=$API_TOKEN\"",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := st.RunScript(ctx, "report", "run.sh", nil,
		map[string]string{"API_TOKEN": "sk-secret-xyz", "CLUSTER": "prod-east"}, sandbox.Policy{}, Allowlist{})
	if err != nil {
		t.Fatalf("run: %v (out=%q)", err, out)
	}
	if want := "3 pods ready in ${CLUSTER}, token=${API_TOKEN}"; !strings.Contains(out, want) {
		t.Fatalf("out = %q, want it to contain %q", out, want)
	}
}
