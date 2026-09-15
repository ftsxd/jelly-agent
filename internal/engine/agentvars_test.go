package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/sandbox"
	"github.com/jelly-agent/jelly-agent/internal/skill"
)

func varsEngine() *Engine {
	return New(&config.Config{
		SkillVars: map[string]map[string]string{
			"deploy": {"API_TOKEN": "skill-token", "REGISTRY": "registry.internal"},
		},
		AgentVars: map[string]map[string]string{
			"ops": {"API_TOKEN": "ops-token", "CLUSTER": "prod"},
		},
	})
}

// The agent is the more specific of the two, so it wins a name collision —
// which is the whole point: one skill shared by several agents, each running it
// with its own credential.
func TestVarsForAgentOverridesSkill(t *testing.T) {
	got := varsEngine().VarsFor("ops", "deploy")

	want := map[string]string{
		"API_TOKEN": "ops-token",         // agent wins
		"REGISTRY":  "registry.internal", // skill-only key survives
		"CLUSTER":   "prod",              // agent-only key is added
	}
	if len(got) != len(want) {
		t.Fatalf("VarsFor = %+v, want %+v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("VarsFor[%q] = %q, want %q", k, got[k], v)
		}
	}
}

// The result is handed to the sandbox. A map the config still holds must not be
// reachable from there — the old closure returned cfg.SkillVars[skill] directly,
// and a merge that wrote into it would rewrite the running configuration.
func TestVarsForDoesNotAliasConfig(t *testing.T) {
	e := varsEngine()

	got := e.VarsFor("ops", "deploy")
	got["API_TOKEN"] = "mutated"
	got["EXTRA"] = "added"

	if v := e.cfg.SkillVars["deploy"]["API_TOKEN"]; v != "skill-token" {
		t.Errorf("skill_vars mutated through the returned map: %q", v)
	}
	if v := e.cfg.AgentVars["ops"]["API_TOKEN"]; v != "ops-token" {
		t.Errorf("agent_vars mutated through the returned map: %q", v)
	}
	if _, ok := e.cfg.SkillVars["deploy"]["EXTRA"]; ok {
		t.Error("a key added to the result leaked into skill_vars")
	}
}

// Every caller that predates agent_vars — the legacy single "root" node built
// by BuildAgentWith for platform bots, the CLI — must keep seeing exactly the
// skill's own variables.
func TestVarsForUnknownAgentKeepsSkillVars(t *testing.T) {
	e := varsEngine()
	for _, name := range []string{"", "root", "unknown-agent"} {
		got := e.VarsFor(name, "deploy")
		if len(got) != 2 || got["API_TOKEN"] != "skill-token" || got["REGISTRY"] != "registry.internal" {
			t.Errorf("VarsFor(%q, deploy) = %+v, want the skill's own vars", name, got)
		}
	}
}

// nil rather than an empty map, so use_skill keeps omitting var_keys entirely
// instead of reporting an empty list.
func TestVarsForNilWhenNothingConfigured(t *testing.T) {
	if got := varsEngine().VarsFor("ops", "unconfigured-skill"); got == nil {
		t.Error("an agent with vars must still get them for a skill that has none")
	}
	if got := varsEngine().VarsFor("no-such-agent", "unconfigured-skill"); got != nil {
		t.Errorf("VarsFor with nothing configured = %+v, want nil", got)
	}
	if got := New(&config.Config{}).VarsFor("ops", "deploy"); got != nil {
		t.Errorf("VarsFor on an empty config = %+v, want nil", got)
	}
}

// The unit tests above check the merge; this one checks it survives the trip
// into a real child process, which is the only thing the feature actually
// promises. ${ENV} is resolved by config.Load long before this point, so what
// reaches the sandbox is a plain value either way.
//
// The script reports a comparison rather than printing the values: the sandbox
// masks any injected value out of the output (that is the point of
// sandbox.redactInjected), so "ops-token" and "skill-token" would both come
// back as ${API_TOKEN} and the test could not tell which one the process saw.
func TestVarsForReachTheScriptEnvironment(t *testing.T) {
	dir := t.TempDir()
	st, err := skill.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(dir, "deploy")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: deploy\ndescription: t\nenabled: true\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"[ \"$API_TOKEN\" = \"ops-token\" ] && echo token=agent || echo token=skill\n" +
		"[ \"$REGISTRY\" = \"registry.internal\" ] && echo registry=ok || echo registry=missing\n" +
		"[ \"$CLUSTER\" = \"prod\" ] && echo cluster=ok || echo cluster=missing\n"
	if err := os.WriteFile(filepath.Join(skillDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := st.RunScript(ctx, "deploy", "run.sh", nil,
		varsEngine().VarsFor("ops", "deploy"), sandbox.Policy{}, skill.Allowlist{})
	if err != nil {
		t.Fatalf("run: %v (out=%q)", err, out)
	}
	for _, want := range []string{"token=agent", "registry=ok", "cluster=ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("script saw %q, want it to contain %q", out, want)
		}
	}
	// And the values themselves must not have made it back out.
	for _, secret := range []string{"ops-token", "skill-token", "registry.internal"} {
		if strings.Contains(out, secret) {
			t.Errorf("value %q reached the caller: %q", secret, out)
		}
	}
}
