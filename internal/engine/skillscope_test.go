package engine

import (
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

// Every agent defined before the skills field existed must keep seeing every
// skill. That is the whole reason the field is a pointer, so it is worth an
// explicit test rather than an inference from the Allowlist unit tests.
func TestSkillsForDefaultsToEverything(t *testing.T) {
	none := []string{}
	some := []string{"pod-check"}
	e := New(&config.Config{Agents: []config.AgentDef{
		{Name: "Legacy", Enabled: true},                     // field absent
		{Name: "Coordinator", Enabled: true, Skills: &none}, // deliberately nothing
		{Name: "Specialist", Enabled: true, Skills: &some},
	}})

	if a := e.SkillsFor("Legacy"); !a.Unrestricted() {
		t.Error("an agent without a skills field must see every skill")
	}
	if a := e.SkillsFor("Coordinator"); !a.DeniesAll() || a.Permits("pod-check") {
		t.Error("an agent with an empty skills list must get none")
	}
	a := e.SkillsFor("Specialist")
	if !a.Permits("pod-check") || a.Permits("db-slow-query") {
		t.Error("a named list must admit exactly its own skills")
	}

	// Callers that do not track agents — the legacy single "root" agent, the
	// CLI — must not be narrowed by accident.
	for _, name := range []string{"", "root", "unknown-agent"} {
		if !e.SkillsFor(name).Unrestricted() {
			t.Errorf("SkillsFor(%q) narrowed an agent it does not know about", name)
		}
	}
}
