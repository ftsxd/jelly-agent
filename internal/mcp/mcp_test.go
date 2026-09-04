package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

// A stdio server has to survive being connected more than once.
//
// An exec.Cmd is single-use — Connect takes its stdin and stdout pipes — so a
// transport holding one Cmd works exactly once and then fails permanently with
// "exec: Stdout already set". One connection is not what happens in practice:
// the toolset lists tools on one session and calls tools on another, so the
// very first tool call hit the second connection and died.
func TestStdioTransportReconnects(t *testing.T) {
	script := filepath.Join(t.TempDir(), "echo_server.py")
	// Answers initialize and then exits when stdin closes, which is all a
	// connection needs to be established and torn down.
	if err := os.WriteFile(script, []byte(`import json,sys
for line in sys.stdin:
    line=line.strip()
    if not line: continue
    req=json.loads(line)
    if req.get("method")=="initialize":
        sys.stdout.write(json.dumps({"jsonrpc":"2.0","id":req.get("id"),"result":{
          "protocolVersion":"2024-11-05","capabilities":{"tools":{}},
          "serverInfo":{"name":"probe","version":"0"}}})+"\n")
        sys.stdout.flush()
`), 0o755); err != nil {
		t.Fatal(err)
	}

	tr, err := Transport(context.Background(), config.MCPServer{
		Name: "probe", Transport: "stdio", Command: pythonPath(t), Args: []string{script},
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 3; i++ {
		conn, err := tr.Connect(t.Context())
		if err != nil {
			t.Fatalf("connection %d failed: %v (a single-use exec.Cmd fails here on the second connect)", i, err)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("connection %d close: %v", i, err)
		}
	}
}

// Env from config must reach the subprocess, since that is how the synthetic
// log server is sized and how a real server is pointed at a cluster.
func TestStdioTransportPassesEnv(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "seen.txt")
	script := filepath.Join(dir, "env_server.py")
	if err := os.WriteFile(script, []byte(`import json,os,sys
open(os.environ["PROBE_OUT"],"w").write(os.environ.get("PROBE_VALUE","<unset>"))
for line in sys.stdin:
    req=json.loads(line)
    if req.get("method")=="initialize":
        sys.stdout.write(json.dumps({"jsonrpc":"2.0","id":req.get("id"),"result":{
          "protocolVersion":"2024-11-05","capabilities":{"tools":{}},
          "serverInfo":{"name":"probe","version":"0"}}})+"\n")
        sys.stdout.flush()
`), 0o755); err != nil {
		t.Fatal(err)
	}

	tr, err := Transport(context.Background(), config.MCPServer{
		Name: "probe", Transport: "stdio", Command: pythonPath(t), Args: []string{script},
		Env: map[string]string{"PROBE_VALUE": "42", "PROBE_OUT": out},
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tr.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Connect starts the process; it does not wait for it to do anything, so
	// the file appears a moment later.
	var b []byte
	for i := 0; i < 200; i++ {
		if b, err = os.ReadFile(out); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the subprocess never reported its environment: %v", err)
	}
	if strings.TrimSpace(string(b)) != "42" {
		t.Errorf("subprocess saw PROBE_VALUE=%q, want 42", b)
	}
}

func pythonPath(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/usr/bin/python3", "/usr/local/bin/python3"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no python3 available")
	return ""
}
