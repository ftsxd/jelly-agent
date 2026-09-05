// Package mcp integrates external Model Context Protocol servers into
// jelly-agent. It builds an ADK toolset from a config.MCPServer (so the agent
// can call the server's tools) and offers a direct connect-and-list helper used
// by the dashboard to preview/test a server without an agent run.
//
// Supported transports: "stdio" (launch a local command, talk over its
// stdin/stdout) and "http"/"sse" (connect to a remote endpoint). stdio commands
// are spawned with a caller-owned context so cancelling it kills the subprocess.
package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

// clientName/Version identify jelly-agent in the MCP handshake.
const (
	clientName    = "jelly-agent"
	clientVersion = "0.2.0"
)

// Transport builds the MCP transport for a server. For stdio the command is
// launched with ctx (cancel ctx to terminate the subprocess); for http/sse a
// header-injecting HTTP client is used when headers are configured.
func Transport(ctx context.Context, srv config.MCPServer) (mcp.Transport, error) {
	switch strings.ToLower(strings.TrimSpace(srv.Transport)) {
	case "", "stdio":
		if strings.TrimSpace(srv.Command) == "" {
			return nil, fmt.Errorf("stdio MCP 服务器 %q 缺少 command", srv.Name)
		}
		env := os.Environ()
		for k, v := range srv.Env {
			env = append(env, k+"="+v)
		}
		return &stdioTransport{ctx: ctx, path: srv.Command, args: srv.Args, env: env}, nil
	case "http", "streamable", "streamable-http":
		if strings.TrimSpace(srv.URL) == "" {
			return nil, fmt.Errorf("http MCP 服务器 %q 缺少 url", srv.Name)
		}
		return &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: httpClient(srv.Headers)}, nil
	case "sse":
		if strings.TrimSpace(srv.URL) == "" {
			return nil, fmt.Errorf("sse MCP 服务器 %q 缺少 url", srv.Name)
		}
		return &mcp.SSEClientTransport{Endpoint: srv.URL, HTTPClient: httpClient(srv.Headers)}, nil
	default:
		return nil, fmt.Errorf("不支持的 MCP transport %q（支持 stdio / http / sse）", srv.Transport)
	}
}

// Toolset returns an ADK toolset for the server, suitable for llmagent.Config
// Toolsets. The connection is established lazily on first use.
//
// A configured tool whitelist is applied at fetch time, so an unwanted tool
// never becomes a tool at all: it claims no schema slot, adds no name for the
// model to confuse with a similar one, and cannot collide with another
// server's. This is the cheapest of the cuts that keep prompt size independent
// of how many servers are registered.
//
// RequireConfirmation is deliberately left off. Approval belongs to the
// gateway, which decides it from the tool's declared side-effect level; ADK's
// own confirmation wrapper would sit outside ours and re-register the request
// entry under its own name (tool/tool.go), so the two would fight over
// dispatch.
func Toolset(ctx context.Context, srv config.MCPServer) (adktool.Toolset, error) {
	tr, err := Transport(ctx, srv)
	if err != nil {
		return nil, err
	}
	cfg := mcptoolset.Config{Transport: tr}
	if len(srv.Tools) > 0 {
		allowed := make(map[string]bool, len(srv.Tools))
		for _, n := range srv.Tools {
			allowed[n] = true
		}
		cfg.ToolFilter = func(_ agent.ReadonlyContext, t adktool.Tool) bool {
			return allowed[t.Name()]
		}
	}
	return mcptoolset.New(cfg)
}

// ToolInfo is a tool advertised by an MCP server, for display.
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListTools connects to the server, lists its tools, and closes the session. It
// is used by the dashboard's "test connection" / tool preview, independent of
// any agent run. A short default timeout guards against hanging servers.
func ListTools(ctx context.Context, srv config.MCPServer) ([]ToolInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	tr, err := Transport(ctx, srv)
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: clientVersion}, nil)
	session, err := client.Connect(ctx, tr, nil)
	if err != nil {
		return nil, fmt.Errorf("连接 MCP 服务器失败: %w", err)
	}
	defer session.Close()

	res, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, fmt.Errorf("列出 MCP 工具失败: %w", err)
	}
	out := make([]ToolInfo, 0, len(res.Tools))
	for _, t := range res.Tools {
		out = append(out, ToolInfo{Name: t.Name, Description: t.Description})
	}
	return out, nil
}

// httpClient returns an HTTP client that injects the given headers on every
// request (e.g. Authorization for a remote MCP server). Returns nil when there
// are no headers, so the SDK uses its default client.
// dialTimeout bounds how long a connection attempt waits.
//
// http.DefaultTransport allows thirty seconds, and the tool list is fetched
// once per user message — so a server that has gone away made every turn
// thirty seconds slower, including turns that would never have called it.
// Five seconds is generous for a reachable server and quick enough that a
// dead one is noticed rather than waited on. It pairs with the cooldown in
// the engine, which stops the attempt being repeated at all for a while.
const dialTimeout = 5 * time.Second

// transport builds the HTTP transport used for MCP over http/sse.
//
// Cloned from the default rather than constructed from scratch, so proxy
// settings, HTTP/2 and connection pooling keep working; only the dial
// deadline is ours.
func transport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext}
	}
	t := base.Clone()
	// Connecting is bounded; the exchange is not.
	//
	// These two get conflated easily and the consequences are opposite. A
	// host that is gone fails at dial, and waiting thirty seconds for that on
	// every turn is pure loss. A tool that takes a minute to answer is doing
	// its job — a log query over a wide window is exactly that — and cutting
	// it at the transport turns a slow success into a failure the model then
	// reports as "工具调用失败".
	//
	// So no ResponseHeaderTimeout and no client-level Timeout. What bounds a
	// call is the tool's own declared timeout in the gateway, which is where
	// the caller can see and set it, and the invocation's context, which ends
	// when the user goes away.
	t.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = dialTimeout
	return t
}

func httpClient(headers map[string]string) *http.Client {
	c := &http.Client{Transport: transport()}
	if len(headers) > 0 {
		c.Transport = &headerRoundTripper{headers: headers, base: c.Transport}
	}
	return c
}

type headerRoundTripper struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

// stdioTransport launches a fresh subprocess for every connection.
//
// An exec.Cmd is single-use: Connect takes its stdout and stdin pipes, so
// reconnecting with the same Cmd fails with "exec: Stdout already set" and the
// server is dead for the rest of the process's life. Handing one Cmd to
// mcp.CommandTransport therefore works exactly once — and one connection is
// not what happens, because the toolset lists tools on one session and calls
// tools on another.
//
// It went unnoticed because the deployed MCP server speaks HTTP; the first
// stdio server pointed at this failed on its first tool call, reporting an
// infrastructure error the model then diagnosed at length for the user.
type stdioTransport struct {
	// ctx bounds the subprocess's life rather than the connection's, which is
	// what makes cancelling the engine's MCP context terminate the children.
	ctx  context.Context
	path string
	args []string
	env  []string
}

func (t *stdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	cmd := exec.CommandContext(t.ctx, t.path, t.args...)
	cmd.Env = t.env
	return (&mcp.CommandTransport{Command: cmd}).Connect(ctx)
}
