package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireOS skips a test on platforms (or kernels) without a system sandbox —
// the assertions below are about what the os backend actually enforces, and
// there is nothing to assert when it degraded to native.
func requireOS(t *testing.T) {
	t.Helper()
	if !osAvailable() {
		t.Skipf("os 后端不可用：%s", osUnavailable())
	}
}

func osPolicy(m Mode) Policy { return Policy{Backend: "os", Mode: m} }

// runOSScript runs a /bin/sh script under the os backend and returns the result.
func runOSScript(t *testing.T, pol Policy, body string, env map[string]string) Result {
	t.Helper()
	dir, name := writeScript(t, "s.sh", body)
	res, err := Run(context.Background(), pol, Spec{Dir: dir, Interp: "sh", RelFile: name, Env: env})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Backend != "os" {
		t.Fatalf("backend = %q (degraded: %s), want os", res.Backend, res.Degraded)
	}
	return res
}

// The workspace is writable; everything outside it is not. This is the one
// property that separates the os backend from native.
func TestOSBackendConfinesWrites(t *testing.T) {
	requireOS(t)
	outside := filepath.Join(t.TempDir(), "escaped.txt")
	res := runOSScript(t, osPolicy(ModeWorkspace),
		"#!/bin/sh\necho in > ./inside.txt && echo INSIDE_OK\necho out > \"$TARGET\" && echo ESCAPED\nexit 0",
		map[string]string{"TARGET": outside})

	if !strings.Contains(res.Output, "INSIDE_OK") {
		t.Fatalf("workspace write was blocked, sandbox is too tight: %q", res.Output)
	}
	if strings.Contains(res.Output, "ESCAPED") {
		t.Fatalf("write outside the workspace succeeded: %q", res.Output)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("file was created outside the workspace")
	}
}

// read-only mode must refuse even the workspace.
func TestOSBackendReadOnlyModeBlocksWorkspaceWrite(t *testing.T) {
	requireOS(t)
	res := runOSScript(t, osPolicy(ModeReadOnly),
		"#!/bin/sh\necho x > ./nope.txt && echo WROTE\nexit 0", nil)
	if strings.Contains(res.Output, "WROTE") {
		t.Fatalf("read-only mode allowed a write: %q", res.Output)
	}
}

// Reads outside the system allowlist are denied — that is what keeps the agent's
// own config and state (API keys, sessions) out of a script's reach, given that
// they sit next to it on the same host.
func TestOSBackendConfinesReads(t *testing.T) {
	requireOS(t)
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "config.yaml")
	if err := os.WriteFile(secret, []byte("api_key: TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := runOSScript(t, osPolicy(ModeWorkspace),
		"#!/bin/sh\ncat \"$SECRET\"\nexit 0", map[string]string{"SECRET": secret})
	if strings.Contains(res.Output, "TOPSECRET") {
		t.Fatalf("script read a file outside the allowlist: %q", res.Output)
	}

	// ...and ReadPaths is the documented way to hand one over on purpose.
	pol := osPolicy(ModeWorkspace)
	pol.ReadPaths = []string{secretDir}
	res = runOSScript(t, pol, "#!/bin/sh\ncat \"$SECRET\"\nexit 0", map[string]string{"SECRET": secret})
	if !strings.Contains(res.Output, "TOPSECRET") {
		t.Fatalf("ReadPaths did not grant the read: %q", res.Output)
	}
}

// Network denial is checked against a loopback server, so the test needs no
// internet and cannot be fooled by a DNS failure looking like a block.
func TestOSBackendNetworkFollowsMode(t *testing.T) {
	requireOS(t)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl to probe with")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("REACHED"))
	}))
	defer srv.Close()

	body := "#!/bin/sh\ncurl -s -m 5 \"$URL\"\nexit 0"
	env := map[string]string{"URL": srv.URL}

	if res := runOSScript(t, osPolicy(ModeWorkspace), body, env); strings.Contains(res.Output, "REACHED") {
		t.Fatalf("workspace mode reached the network: %q", res.Output)
	}
	if res := runOSScript(t, osPolicy(ModeWorkspaceNet), body, env); !strings.Contains(res.Output, "REACHED") {
		t.Fatalf("workspace-net mode could not reach the network: %q", res.Output)
	}
}

// The sandbox must not be so tight that ordinary scripts stop working: a real
// interpreter has to start, import its standard library and print.
func TestOSBackendRunsPythonCleanly(t *testing.T) {
	requireOS(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	dir, name := writeScript(t, "t.py", "import json, os, sys\nprint('PY', json.dumps({'cwd': os.path.basename(os.getcwd())}))\n")
	res, err := Run(context.Background(), osPolicy(ModeWorkspace), Spec{Dir: dir, Interp: "python3", RelFile: name})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 || !strings.HasPrefix(strings.TrimSpace(res.Output), "PY ") {
		t.Fatalf("python could not run cleanly under the sandbox: exit=%d output=%q", res.ExitCode, res.Output)
	}
}

// The audit trail has to carry the envelope, not just the command: a reviewer
// asking "what was this allowed to do" must not have to guess.
func TestAuditCarriesModeAndBackend(t *testing.T) {
	requireOS(t)
	var got AuditEvent
	prev := Audit
	Audit = func(ev AuditEvent) { got = ev }
	defer func() { Audit = prev }()

	runOSScript(t, osPolicy(ModeReadOnly), "#!/bin/sh\nexit 0", nil)
	if got.Backend != "os" || got.Mode != ModeReadOnly {
		t.Fatalf("audit event lost the envelope: %+v", got)
	}
}

// A sync script has to write somewhere that is not its own directory. WritePaths
// is how that is said out loud in config, instead of switching the policy off
// for that skill — which is the one skill you least want unconfined, since it
// is the one holding a token and talking to the network.
func TestOSBackendWritePathsOpenExactlyWhatIsListed(t *testing.T) {
	requireOS(t)
	repos := t.TempDir()
	elsewhere := t.TempDir()

	pol := osPolicy(ModeWorkspace)
	pol.WritePaths = []string{repos}
	res := runOSScript(t, pol,
		"#!/bin/sh\necho a > \"$REPOS/ok.txt\" && echo WROTE_REPOS\necho b > \"$OTHER/no.txt\" && echo WROTE_OTHER\nexit 0",
		map[string]string{"REPOS": repos, "OTHER": elsewhere})

	if !strings.Contains(res.Output, "WROTE_REPOS") {
		t.Fatalf("a listed write path was still blocked: %q", res.Output)
	}
	if strings.Contains(res.Output, "WROTE_OTHER") {
		t.Fatalf("an unlisted path was writable: %q", res.Output)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "no.txt")); err == nil {
		t.Fatal("a file appeared outside every configured write path")
	}

	// A path you can write but not read is a trap — git cannot update a
	// checkout it cannot stat — so listing it must grant both.
	res = runOSScript(t, pol, "#!/bin/sh\ncat \"$REPOS/ok.txt\"\nexit 0", map[string]string{"REPOS": repos})
	if !strings.Contains(res.Output, "a") {
		t.Fatalf("a write path was not readable: %q", res.Output)
	}
}

// read-only means read-only: an extra write path does not survive it, or the
// mode would be advisory.
func TestReadOnlyModeIgnoresWritePaths(t *testing.T) {
	requireOS(t)
	repos := t.TempDir()
	pol := osPolicy(ModeReadOnly)
	pol.WritePaths = []string{repos}

	res := runOSScript(t, pol, "#!/bin/sh\necho x > \"$REPOS/nope.txt\" && echo WROTE\nexit 0",
		map[string]string{"REPOS": repos})
	if strings.Contains(res.Output, "WROTE") {
		t.Fatalf("read-only mode honored a write path: %q", res.Output)
	}
}
