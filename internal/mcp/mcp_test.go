package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// Connecting is bounded; the exchange is not.
//
// These two get conflated easily and the consequences are opposite. A host
// that is gone fails at dial, and waiting thirty seconds for that on every
// turn — the tool list is fetched once per user message — is pure loss. A tool
// that takes a minute to answer is doing its job: a log query over a wide
// window is exactly that, and cutting it at the transport turns a slow success
// into a failure the model then reports as "工具调用失败".
//
// The zero values are the decision, so they are asserted as such. What bounds
// a call is the tool's declared timeout in the gateway, where the caller can
// see and set it, and the invocation's context, which ends when the user goes
// away.
func TestTheTransportBoundsConnectingNotTheExchange(t *testing.T) {
	tr := transport()
	if tr.DialContext == nil {
		t.Fatal("no dial deadline; a dead host would cost the default thirty seconds every turn")
	}
	if tr.TLSHandshakeTimeout != dialTimeout {
		t.Errorf("TLS handshake timeout = %v, want %v", tr.TLSHandshakeTimeout, dialTimeout)
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Errorf("ResponseHeaderTimeout = %v; a slow tool would be cut mid-answer and reported as a failure",
			tr.ResponseHeaderTimeout)
	}
	if c := httpClient(nil); c.Timeout != 0 {
		t.Errorf("client Timeout = %v; the same problem one level up", c.Timeout)
	}
	if c := httpClient(map[string]string{"X-Token": "t"}); c.Timeout != 0 {
		t.Errorf("client Timeout with headers = %v", c.Timeout)
	}
	// Pooling and proxy settings have to survive, which is why the transport
	// is cloned from the default rather than built from scratch.
	if tr.MaxIdleConns == 0 || tr.Proxy == nil {
		t.Error("the transport was built from scratch; proxy and pooling settings were lost")
	}
}

// A slow-but-alive server must complete, not be cut.
func TestASlowServerIsNotCutOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(250 * time.Millisecond) // long enough that any per-request cap would show
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	resp, err := httpClient(map[string]string{"X-Token": "t"}).Get(srv.URL)
	if err != nil {
		t.Fatalf("a server that took 250ms was cut off: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// A host that is not there must fail at the dial deadline rather than at the
// operating system's, which is what made every turn thirty seconds slower.
func TestAnUnreachableHostFailsAtTheDialDeadline(t *testing.T) {
	// TEST-NET-1 (RFC 5737): reserved for documentation, never routed.
	start := time.Now()
	_, err := httpClient(nil).Get("http://192.0.2.1:443/mcp")
	if err == nil {
		t.Fatal("a reserved, unroutable address answered")
	}
	if took := time.Since(start); took > dialTimeout+3*time.Second {
		t.Errorf("dial took %v, want it bounded near %v", took, dialTimeout)
	}
}
