package server

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

// agentNameRe constrains agent identifiers to the same charset as skills, so a
// name is safe in config, URLs and as the agent's tree name.
var agentNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// handleListAgents returns the defined agents and the default agent name so the
// web "Agents" page can render the coordinator/sub-agent tree.
func (s *Server) handleListAgents(w http.ResponseWriter, _ *http.Request) {
	cfg := s.engine().Config()
	agents := cfg.Agents
	if agents == nil {
		agents = []config.AgentDef{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents":        agents,
		"default_agent": cfg.DefaultAgent,
	})
}

// agentInput is the POST /api/agents body (create or update by name).
type agentInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Provider    string   `json:"provider"`
	Instruction string   `json:"instruction"`
	MCP         []string `json:"mcp"`
	SubAgents   []string `json:"sub_agents"`
	Enabled     bool     `json:"enabled"`
	MakeDefault bool     `json:"make_default"`
}

// handleSaveAgent upserts an agent definition and hot-reloads. Sub-agent names
// are validated against the saved set (after this upsert) so a coordinator can
// never reference a missing or self child.
func (s *Server) handleSaveAgent(w http.ResponseWriter, r *http.Request) {
	var in agentInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !agentNameRe.MatchString(in.Name) {
		writeErr(w, http.StatusBadRequest, "name 只能含字母/数字/下划线/连字符且不能为空")
		return
	}
	subs := cleanNames(in.SubAgents)
	for _, sub := range subs {
		if sub == in.Name {
			writeErr(w, http.StatusBadRequest, "agent 不能把自己列为子 agent")
			return
		}
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

	def := config.AgentDef{
		Name:        in.Name,
		Description: strings.TrimSpace(in.Description),
		Provider:    strings.TrimSpace(in.Provider),
		Instruction: in.Instruction,
		MCP:         cleanNames(in.MCP),
		SubAgents:   subs,
		Enabled:     in.Enabled,
	}
	if idx := indexOfAgent(raw.Agents, in.Name); idx < 0 {
		raw.Agents = append(raw.Agents, def)
	} else {
		raw.Agents[idx] = def
	}

	// Validate sub-agent references now that the set is final.
	for _, sub := range subs {
		if indexOfAgent(raw.Agents, sub) < 0 {
			writeErr(w, http.StatusBadRequest, "子 agent 不存在: "+sub)
			return
		}
	}

	if in.MakeDefault || raw.DefaultAgent == "" {
		raw.DefaultAgent = in.Name
	}

	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}

// handleDeleteAgent removes an agent, scrubs it from other agents' sub_agents,
// clears default_agent if it pointed here, and hot-reloads.
func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
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
	idx := indexOfAgent(raw.Agents, name)
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "agent 不存在")
		return
	}
	raw.Agents = append(raw.Agents[:idx], raw.Agents[idx+1:]...)
	for i := range raw.Agents {
		raw.Agents[i].SubAgents = removeName(raw.Agents[i].SubAgents, name)
	}
	if raw.DefaultAgent == name {
		raw.DefaultAgent = ""
	}
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}

func indexOfAgent(as []config.AgentDef, name string) int {
	for i, a := range as {
		if a.Name == name {
			return i
		}
	}
	return -1
}

// cleanNames trims, drops blanks, and de-duplicates a name list, preserving
// order. Returns nil for an all-empty input so omitempty keeps config tidy.
func cleanNames(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func removeName(in []string, name string) []string {
	var out []string
	for _, n := range in {
		if n != name {
			out = append(out, n)
		}
	}
	return out
}
