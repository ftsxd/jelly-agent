package server

import (
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/config"
	jellymcp "github.com/jelly-agent/jelly-agent/internal/mcp"
	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// mcpInput is the body for POST /api/mcp (create/update) and /api/mcp/test.
type mcpInput struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	Enabled   bool              `json:"enabled"`

	// Tools whitelists which of the server's tools to load. A nil value means
	// "leave whatever is stored alone" — distinct from an empty array, which
	// means "load everything". Without that distinction, saving any unrelated
	// field from the console would wipe a whitelist written by hand.
	Tools []string `json:"tools"`
}

// handleListMCP lists configured MCP servers. Secret values (env/headers) are
// never sent to the browser — only their keys, so the UI can show what's set
// without leaking tokens.
func (s *Server) handleListMCP(w http.ResponseWriter, _ *http.Request) {
	type mcpDTO struct {
		Name       string   `json:"name"`
		Transport  string   `json:"transport"`
		Command    string   `json:"command,omitempty"`
		Args       []string `json:"args,omitempty"`
		URL        string   `json:"url,omitempty"`
		EnvKeys    []string `json:"env_keys,omitempty"`
		HeaderKeys []string `json:"header_keys,omitempty"`
		Enabled    bool     `json:"enabled"`
		Tools      []string `json:"tools,omitempty"`
	}
	servers := s.engine().Config().MCP
	out := make([]mcpDTO, 0, len(servers))
	for _, m := range servers {
		out = append(out, mcpDTO{
			Name: m.Name, Transport: m.Transport, Command: m.Command, Args: m.Args,
			URL: m.URL, EnvKeys: sortedKeys(m.Env), HeaderKeys: sortedKeys(m.Headers),
			Enabled: m.Enabled, Tools: m.Tools,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}

// handleSaveMCP upserts an MCP server and hot-reloads. Like provider keys, env
// and header values are merged: an empty value on update keeps the stored one
// (including ${ENV} refs), so editing an unrelated field never drops a secret.
func (s *Server) handleSaveMCP(w http.ResponseWriter, r *http.Request) {
	var in mcpInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	srv, err := normalizeMCP(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if srv.Transport == "stdio" && srv.Command == "" {
		writeErr(w, http.StatusBadRequest, "stdio 传输需填写 command")
		return
	}
	if (srv.Transport == "http" || srv.Transport == "sse") && srv.URL == "" {
		writeErr(w, http.StatusBadRequest, "http/sse 传输需填写 url")
		return
	}

	path, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := loadRawOrEmpty(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	if idx := indexOfMCP(raw.MCP, srv.Name); idx >= 0 {
		srv.Env = mergeSecrets(raw.MCP[idx].Env, srv.Env)
		srv.Headers = mergeSecrets(raw.MCP[idx].Headers, srv.Headers)
		// A nil Tools means the caller said nothing about the whitelist, so
		// keep the stored one. This is the same trap config.Save has: a
		// handler that rebuilds the whole record from its input silently drops
		// every field the input does not carry, and the symptom — a whitelist
		// that vanishes when someone toggles "enabled" — points nowhere near
		// the cause.
		if srv.Tools == nil {
			srv.Tools = raw.MCP[idx].Tools
		}
		raw.MCP[idx] = srv
	} else {
		raw.MCP = append(raw.MCP, srv)
	}

	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}

// handleDeleteMCP removes an MCP server and hot-reloads.
func (s *Server) handleDeleteMCP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := config.LoadRaw(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeErr(w, http.StatusNotFound, "尚无配置文件可删除")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := indexOfMCP(raw.MCP, name)
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "MCP 服务器不存在")
		return
	}
	raw.MCP = append(raw.MCP[:idx], raw.MCP[idx+1:]...)
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}

// handleTestMCP connects to a server and lists its tools, without saving. The
// body may be a full inline spec (test before saving) or just a name to test an
// already-configured server (using its expanded, secret-resolved config).
func (s *Server) handleTestMCP(w http.ResponseWriter, r *http.Request) {
	var in mcpInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	srv, err := normalizeMCP(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Inline spec lacks endpoint details → fall back to the configured server,
	// whose env/header secrets are already expanded in the running engine.
	if srv.Command == "" && srv.URL == "" {
		if existing, ok := findMCP(s.engine().Config().MCP, srv.Name); ok {
			srv = existing
		}
	}

	tools, err := jellymcp.ListTools(r.Context(), srv)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	// Report name clashes here, where someone is already looking at this
	// server's tools and can act on it.
	//
	// The alternative is what ADK does on its own: fail while assembling a
	// request, several turns after the server was added, with a message that
	// names neither culprit. Two Kubernetes servers both exposing get_pods is
	// not an edge case — it is what happens the moment a second environment is
	// added.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"tools":     tools,
		"conflicts": s.toolConflicts(srv.Name, tools),
	})
}

// conflictReport is one clash, for the console to render next to the tool.
type conflictReport struct {
	Tool   string `json:"tool"`   // the server's own name for it
	Reason string `json:"reason"` // what it collides with, both sides named
}

// toolConflicts reports which of a server's tools cannot be registered under
// their own names.
//
// Tools already registered for this same server are excluded from the check:
// re-testing a configured server would otherwise report every one of its tools
// as conflicting with itself.
func (s *Server) toolConflicts(server string, tools []jellymcp.ToolInfo) []conflictReport {
	reg := s.engine().ToolRegistry()
	if reg == nil || len(tools) == 0 {
		return nil
	}

	incoming := make([]ops.ToolMetadata, 0, len(tools))
	for _, t := range tools {
		incoming = append(incoming, ops.ToolMetadata{
			Name: t.Name, RemoteName: t.Name, Server: server, Description: t.Description,
		})
	}

	var out []conflictReport
	for _, c := range reg.CheckAgainst(incoming) {
		if c.ExistingKey == "" {
			continue
		}
		// Skip a clash with this same server: that is the entry being retested.
		if strings.HasPrefix(c.ExistingKey, server+"/") {
			continue
		}
		out = append(out, conflictReport{Tool: c.Name, Reason: c.Error()})
	}
	return out
}

// normalizeMCP validates and canonicalizes an MCP input into a config server.
func normalizeMCP(in mcpInput) (config.MCPServer, error) {
	srv := config.MCPServer{
		Name:      strings.TrimSpace(in.Name),
		Transport: strings.ToLower(strings.TrimSpace(in.Transport)),
		Command:   strings.TrimSpace(in.Command),
		Args:      in.Args,
		Env:       in.Env,
		URL:       strings.TrimSpace(in.URL),
		Headers:   in.Headers,
		Enabled:   in.Enabled,
		Tools:     in.Tools,
	}
	if srv.Name == "" {
		return srv, errBadInput("name 不能为空")
	}
	if srv.Transport == "" {
		srv.Transport = "stdio"
	}
	switch srv.Transport {
	case "stdio":
		// command may be empty here for a name-only test; save-path validates below.
	case "http", "sse":
	default:
		return srv, errBadInput("transport 仅支持 stdio / http / sse")
	}
	return srv, nil
}

// mergeSecrets returns existing overlaid with submitted: a non-empty submitted
// value overwrites, an empty/absent one keeps the existing value.
func mergeSecrets(existing, submitted map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range submitted {
		if strings.TrimSpace(v) != "" {
			out[k] = v
		} else if _, ok := out[k]; !ok {
			out[k] = v // new key explicitly set empty
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func indexOfMCP(ms []config.MCPServer, name string) int {
	for i, m := range ms {
		if m.Name == name {
			return i
		}
	}
	return -1
}

func findMCP(ms []config.MCPServer, name string) (config.MCPServer, bool) {
	if i := indexOfMCP(ms, name); i >= 0 {
		return ms[i], true
	}
	return config.MCPServer{}, false
}

type badInput string

func (e badInput) Error() string { return string(e) }
func errBadInput(s string) error { return badInput(s) }
