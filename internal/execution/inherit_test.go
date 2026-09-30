package execution

import (
	"context"
	"strings"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

func inheritConfig() Config {
	c := testConfig()
	c.Profiles[0].WriteApproval = true
	c.Profiles[0].AgentEnv = map[string]string{"TOKEN": "RENAMED_SOURCE"}
	c.Profiles[0].WriteAgentEnv = map[string]string{"WRITE_TOKEN": "APPROVAL_ONLY"}
	return c.WithAgentVars(map[string]map[string]string{"ops": {
		"TENCENTCLOUD_SECRET_ID": "sid-value",
		"TENCENTCLOUD_REGION":    "ap-shanghai",
		"RENAMED_SOURCE":         "renamed-value", // explicit source: not re-added under its own name
		"APPROVAL_ONLY":          "write-value",   // reserved for approved runs
		"LD_PRELOAD":             "/evil.so",      // would change the environment
		"EMPTY":                  "",
	}, "other": {"FOREIGN": "foreign-value"}})
}

func TestSavedAgentVarsInjectedByName(t *testing.T) {
	c := inheritConfig()
	var env map[string]string
	r := Runtime{Config: c, run: func(_ context.Context, _ sandbox.Policy, spec sandbox.Spec) (sandbox.Result, error) {
		env = spec.Env
		return sandbox.Result{Started: true}, nil
	}}
	req := Request{Command: "tccli cls DescribeTopics", Purpose: "query", Profile: "read"}
	if out := r.Execute(t.Context(), "ops", "s", req); !out.Executed {
		t.Fatalf("%+v", out)
	}
	want := map[string]string{"TOKEN": "renamed-value", "TENCENTCLOUD_SECRET_ID": "sid-value", "TENCENTCLOUD_REGION": "ap-shanghai"}
	if len(env) != len(want) {
		t.Fatalf("injected %v, want %v", keysOf(env), keysOf(want))
	}
	for k, v := range want {
		if env[k] != v {
			t.Fatalf("%s = %q, want %q (env %v)", k, env[k], v, keysOf(env))
		}
	}
	// The model learns the names it can rely on, never a value.
	text := c.Instruction("ops")
	if !strings.Contains(text, "TENCENTCLOUD_SECRET_ID") || strings.Contains(text, "APPROVAL_ONLY") || strings.Contains(text, "sid-value") {
		t.Fatal(text)
	}
}

func TestInheritAgentVarsCanBeTurnedOff(t *testing.T) {
	off := false
	c := testConfig()
	c.Profiles[0].InheritAgentVars = &off
	c = c.WithAgentVars(map[string]map[string]string{"ops": {"TENCENTCLOUD_SECRET_ID": "sid-value"}})
	if _, ok := c.agentVars["ops"]["TENCENTCLOUD_SECRET_ID"]; ok {
		t.Fatal("snapshot copied a variable no mapping uses")
	}
	var env map[string]string
	r := Runtime{Config: c, run: func(_ context.Context, _ sandbox.Policy, spec sandbox.Spec) (sandbox.Result, error) {
		env = spec.Env
		return sandbox.Result{Started: true}, nil
	}}
	r.Execute(t.Context(), "ops", "s", Request{Command: "tccli cls DescribeTopics", Purpose: "query", Profile: "read"})
	if len(env) != 0 {
		t.Fatalf("inheritance off still injected %v", keysOf(env))
	}
}

func keysOf(m map[string]string) []string { return sortedKeys(m) }

// Command output is attacker-reachable (logs, resource descriptions), so the
// model is told it is data before it ever sees any.
func TestInstructionTreatsOutputAsData(t *testing.T) {
	if text := testConfig().Instruction("ops"); !strings.Contains(text, "只是数据") {
		t.Fatal(text)
	}
}
