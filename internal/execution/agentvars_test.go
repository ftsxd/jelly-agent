package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
	"gopkg.in/yaml.v3"
)

func TestAgentCredentialSnapshotIsPrivateScopedAndRevocable(t *testing.T) {
	c := approvalConfig()
	c.Profiles[0].AgentEnv = map[string]string{"TOKEN": "CLOUD_KEY"}
	vars := map[string]map[string]string{"ops": {"CLOUD_KEY": "private-ops-value"}, "other": {"CLOUD_KEY": "private-other-value"}}
	c = c.WithAgentVars(vars)
	oldHash := configHash(c)
	vars["ops"]["CLOUD_KEY"] = "rotated-ops-value"
	if c.agentVars["ops"]["CLOUD_KEY"] != "private-ops-value" || oldHash == configHash(c.WithAgentVars(vars)) {
		t.Fatal("snapshot aliases live config or rotation is not bound")
	}
	if _, ok := c.agentVars["other"]; ok {
		t.Fatal("unassigned agent's secret copied")
	}
	j, _ := json.Marshal(c)
	y, _ := yaml.Marshal(c)
	if strings.Contains(string(j)+string(y)+c.Instruction("ops"), "private-ops-value") {
		t.Fatal("private values serialized")
	}
	store := approvalStore(t)
	a := newApproval(t, store, c, approvalRequest())
	if err := store.Resolve(t.Context(), a.ID, "s", "admin", true, c.WithAgentVars(vars)); err == nil {
		t.Fatal("rotated identity approved")
	}
	if sandbox.CheckStrict(sandbox.Policy{Backend: "os", Mode: sandbox.ModeWorkspace}) != nil {
		t.Skip("OS sandbox required")
	}
	r := Runtime{Config: c, run: func(_ context.Context, _ sandbox.Policy, spec sandbox.Spec) (sandbox.Result, error) {
		if spec.Env["TOKEN"] != "private-ops-value" || len(spec.Env) != 1 {
			t.Fatal("wrong credential injection")
		}
		return sandbox.Result{ExitCode: 0, Stdout: spec.Env["TOKEN"]}, nil
	}}
	req := Request{Command: "tccli cls DescribeTopics", Purpose: "query", Profile: "read"}
	if out := r.Execute(t.Context(), "ops", "s", req); !out.Executed || strings.Contains(out.Stdout, "private-ops-value") {
		t.Fatal(out)
	}
	r.Config = c.WithAgentVars(map[string]map[string]string{"other": {"CLOUD_KEY": "foreign"}})
	if out := r.Execute(t.Context(), "ops", "s", req); out.Executed || !strings.Contains(out.Error, "CLOUD_KEY") {
		t.Fatal("missing local identity inherited another agent", out)
	}
}

func TestAgentCredentialMappingsCannotAlterIsolation(t *testing.T) {
	for _, key := range []string{"PATH", "HOME", "PYTHONPATH", "BASH_ENV", "DOCKER_HOST"} {
		c := testConfig()
		c.Profiles[0].AgentEnv = map[string]string{key: "KEY"}
		if c.Validate() == nil {
			t.Fatal("unsafe agent mapping accepted", key)
		}
	}
	c := testConfig()
	c.Profiles[0].Env = map[string]string{"TOKEN": "SERVER"}
	c.Profiles[0].AgentEnv = map[string]string{"TOKEN": "AGENT"}
	if c.Validate() == nil {
		t.Fatal("ambiguous sources accepted")
	}
	c = testConfig()
	c.Profiles[0].WriteAgentEnv = map[string]string{"TOKEN": "WRITE"}
	if c.Validate() == nil {
		t.Fatal("write identity without approval accepted")
	}
	c = approvalConfig()
	c.Profiles[0].WriteAgentEnv = map[string]string{"TOKEN": "WRITE_KEY"}
	c.Profiles = append(c.Profiles, Profile{Name: "second", Agents: []string{"ops"}, AgentEnv: map[string]string{"TOKEN": "WRITE_KEY"}})
	if c.Validate() == nil {
		t.Fatal("approval source borrowed by another automatic profile")
	}
	c.Profiles[1].Agents = []string{"other"}
	if err := c.Validate(); err != nil {
		t.Fatal("different agent's independent source rejected", err)
	}
}

func TestApprovedAgentSourceOverridesServerSourceWithoutReadFallback(t *testing.T) {
	if sandbox.CheckStrict(sandbox.Policy{Backend: "os", Mode: sandbox.ModeWorkspace}) != nil {
		t.Skip("OS sandbox required")
	}
	store := approvalStore(t)
	c := approvalConfig()
	c.Profiles[0].Env = map[string]string{"TOKEN": "MISSING_READ_SOURCE"}
	c.Profiles[0].WriteAgentEnv = map[string]string{"TOKEN": "WRITE"}
	c = c.WithAgentVars(map[string]map[string]string{"ops": {"WRITE": "limited-write-value"}})
	a := newApproval(t, store, c, approvalRequest())
	if err := store.Resolve(t.Context(), a.ID, "s", "admin", true, c); err != nil {
		t.Fatal(err)
	}
	r := Runtime{Config: c, Approvals: &store, run: func(_ context.Context, _ sandbox.Policy, spec sandbox.Spec) (sandbox.Result, error) {
		if spec.Env["TOKEN"] != "limited-write-value" {
			t.Fatal("wrong overlay")
		}
		return sandbox.Result{ExitCode: 0}, nil
	}}
	if out := r.ExecuteApproved(WithApproval(t.Context(), a.ID), "ops", "s", "call", approvalRequest()); !out.Executed {
		t.Fatal(out)
	}
}

func TestToolRuntimeReadsOnlyItsInstalledContents(t *testing.T) {
	if !sandbox.OSSandboxAvailable() {
		t.Skip("OS sandbox required")
	}
	dir := t.TempDir()
	runtime := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(filepath.Join(runtime, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(runtime, "host-config")
	os.WriteFile(private, []byte("host-credential"), 0600)
	script := "#!/bin/sh\n[ -d \"$HOME\" ] || exit 1\nif cat '" + private + "' 2>/dev/null; then exit 2; fi\nprintf 'isolated-ok'\n"
	os.WriteFile(filepath.Join(runtime, "bin", "tccli"), []byte(script), 0700)
	c := testConfig()
	c.Profiles[0].ToolDir = runtime
	out := (Runtime{Config: c}).Execute(t.Context(), "ops", "s", Request{Command: "tccli cls DescribeTopics help", Purpose: "help", Profile: "read"})
	if !out.Executed || out.ExitCode != 0 || out.Stdout != "isolated-ok" {
		t.Fatal(out)
	}
}

func TestToolRuntimeRejectsDirectoryLinksOutsideRuntime(t *testing.T) {
	dir := t.TempDir()
	runtime := filepath.Join(dir, "runtime")
	outside := filepath.Join(dir, "outside")
	os.MkdirAll(filepath.Join(runtime, "bin"), 0700)
	os.MkdirAll(outside, 0700)
	if err := os.Symlink(outside, filepath.Join(runtime, "lib")); err != nil {
		t.Fatal(err)
	}
	if _, err := trustedToolPaths(runtime); err == nil {
		t.Fatal("external library directory granted")
	}
}
