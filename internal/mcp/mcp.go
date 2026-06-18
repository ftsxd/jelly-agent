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
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
		cmd := exec.CommandContext(ctx, srv.Command, srv.Args...)
		cmd.Env = os.Environ()
		for k, v := range srv.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		return &mcp.CommandTransport{Command: cmd}, nil
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
func Toolset(ctx context.Context, srv config.MCPServer) (adktool.Toolset, error) {
	tr, err := Transport(ctx, srv)
	if err != nil {
		return nil, err
	}
	return mcptoolset.New(mcptoolset.Config{Transport: tr})
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
func httpClient(headers map[string]string) *http.Client {
	if len(headers) == 0 {
		return nil
	}
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: &headerRoundTripper{headers: headers, base: http.DefaultTransport},
	}
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
