package engine

import (
	"strings"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/execution"
	"github.com/jelly-agent/jelly-agent/internal/ops"
)

func TestShellExecIsGrantedPerAgentAndDisabledByDefault(t *testing.T) {
	cfg := &config.Config{Execution: execution.Config{Enabled: true, Profiles: []execution.Profile{{Name: "readonly", Agents: []string{"expert"}}}}}
	e := New(cfg)
	t.Cleanup(e.Close)
	for _, name := range []string{"root", "coordinator", "unknown"} {
		if tl, err := e.ShellExecToolFor(name); tl != nil || err != nil {
			t.Fatalf("unassigned %s: %v %v", name, tl, err)
		}
	}
	tl, err := e.ShellExecToolFor("expert")
	if err != nil || tl == nil || tl.Name() != "shell_exec" {
		t.Fatal(tl, err)
	}
	if !strings.Contains(e.executionInstruction("expert"), "readonly") || e.executionInstruction("coordinator") != "" {
		t.Fatal("wrong prompt scope")
	}
	if e.sideEffectCeiling() != ops.SideEffectRisky {
		t.Fatal("gateway rejects configured executor")
	}
	cfg.Execution.Enabled = false
	if tl, err := e.ShellExecToolFor("expert"); tl != nil || err != nil {
		t.Fatal("disabled executor registered")
	}
}

func TestApprovalOnlyAgentVariablesNeverReachSkillScripts(t *testing.T) {
	cfg := &config.Config{AgentVars: map[string]map[string]string{"expert": {"READ": "read-value", "WRITE": "write-value"}}, SkillVars: map[string]map[string]string{"skill": {"WRITE": "also-reserved"}}, Execution: execution.Config{Enabled: true, Profiles: []execution.Profile{{Name: "cloud", Agents: []string{"expert"}, WriteApproval: true, WriteAgentEnv: map[string]string{"TOKEN": "WRITE"}}}}}
	e := New(cfg)
	t.Cleanup(e.Close)
	for _, enabled := range []bool{true, false} {
		cfg.Execution.Enabled = enabled
		vars := e.VarsFor("expert", "skill")
		if vars["READ"] != "read-value" || vars["WRITE"] != "" {
			t.Fatal("write source reached script", enabled)
		}
	}
}
