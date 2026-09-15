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
func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	cfg := s.engineFor(r).Config()
	agents := cfg.Agents
	if agents == nil {
		agents = []config.AgentDef{}
	}
	// Variable NAMES only, as a sibling map rather than a field on AgentDef:
	// the list marshals AgentDef verbatim, so a value stored there would be on
	// its way to the browser before anyone noticed.
	varKeys := map[string][]string{}
	for _, a := range agents {
		if keys := sortedKeys(cfg.AgentVars[a.Name]); len(keys) > 0 {
			varKeys[a.Name] = keys
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents":        agents,
		"default_agent": cfg.DefaultAgent,
		"var_keys":      varKeys,
	})
}

// agentInput is the POST /api/agents body (create or update by name).
type agentInput struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Provider       string   `json:"provider"`
	Instruction    string   `json:"instruction"`
	MCP            []string `json:"mcp"`
	RequiredTools  []string `json:"required_tools"`
	RequiredSuites []string `json:"required_suites"`
	// Skills is a pointer because the three states are distinct and the UI has
	// to be able to say all three: absent/null ⇒ every skill, a list ⇒ those,
	// an empty list ⇒ none. Decoding it as a plain slice would collapse "none"
	// into "all" and make a coordinator undeclarable.
	Skills      *[]string `json:"skills"`
	SubAgents   []string  `json:"sub_agents"`
	Enabled     bool      `json:"enabled"`
	MakeDefault bool      `json:"make_default"`
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

	path, raw, done, err := s.editConfig()
	defer done()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	def := config.AgentDef{
		Name:           in.Name,
		Description:    strings.TrimSpace(in.Description),
		Provider:       strings.TrimSpace(in.Provider),
		Instruction:    in.Instruction,
		MCP:            cleanNames(in.MCP),
		RequiredTools:  cleanNames(in.RequiredTools),
		RequiredSuites: cleanNames(in.RequiredSuites),
		Skills:         cleanNamesPtr(in.Skills),
		SubAgents:      subs,
		Enabled:        in.Enabled,
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
	path, raw, done, err := s.editConfig()
	defer done()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := indexOfAgent(raw.Agents, name)
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "agent 不存在")
		return
	}
	if err := s.engineFor(r).CodeProjects().RevokeAgent(name); err != nil {
		writeErr(w, http.StatusInternalServerError, "无法收回项目授权: "+err.Error())
		return
	}
	raw.Agents = append(raw.Agents[:idx], raw.Agents[idx+1:]...)
	for i := range raw.Agents {
		raw.Agents[i].SubAgents = removeName(raw.Agents[i].SubAgents, name)
	}
	if raw.DefaultAgent == name {
		raw.DefaultAgent = ""
	}
	delete(raw.AgentVars, name) // otherwise a later agent reusing the name inherits them
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}

// envVarNameRe constrains a variable name to what a shell can actually export.
var envVarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnvNames are set by the sandbox itself. scrubEnv lays down its base
// first and appends the injected pairs, so these would silently win — and a
// replaced PATH costs the script its interpreter. Rejecting beats debugging
// why a skill that works everywhere else fails under one agent.
var reservedEnvNames = map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true}

// agentVarsInput sets an agent's variables (key/value). An empty value keeps
// the stored one, so editing without re-typing a secret preserves it.
type agentVarsInput struct {
	Vars map[string]string `json:"vars"`
}

// handleSetAgentVars merges variables into an agent's config-stored set
// (config.yaml, 0600), then persists + hot-reloads. They are injected into that
// agent's run_script sandbox on top of the skill's own vars, the agent winning
// a collision. Values may be ${ENV} references, which stay verbatim in the file
// and resolve from the process environment at load — so a secret need never be
// written down. Only the key names ever reach the model or the browser.
//
// Deliberately a separate endpoint from POST /api/agents: that handler rebuilds
// the whole record from its input, and the console re-posts partial records when
// toggling an agent on or off. Variables living here cannot be erased by a click
// on the power button.
func (s *Server) handleSetAgentVars(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !agentNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "agent 名非法")
		return
	}
	var in agentVarsInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	for k := range in.Vars {
		if !envVarNameRe.MatchString(k) {
			writeErr(w, http.StatusBadRequest, "变量名只能含字母、数字和下划线，且不能以数字开头: "+k)
			return
		}
		if reservedEnvNames[k] {
			writeErr(w, http.StatusBadRequest, "该变量名由沙箱保留，不能覆盖: "+k)
			return
		}
	}
	path, raw, done, err := s.editConfig()
	defer done()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if indexOfAgent(raw.Agents, name) < 0 {
		writeErr(w, http.StatusNotFound, "agent 不存在")
		return
	}
	if raw.AgentVars == nil {
		raw.AgentVars = map[string]map[string]string{}
	}
	raw.AgentVars[name] = mergeSecrets(raw.AgentVars[name], in.Vars) // empty values keep existing
	if len(raw.AgentVars[name]) == 0 {
		delete(raw.AgentVars, name)
	}
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "var_keys": sortedKeys(raw.AgentVars[name])})
}

// handleDeleteAgentVar removes one variable from an agent.
func (s *Server) handleDeleteAgentVar(w http.ResponseWriter, r *http.Request) {
	name, key := r.PathValue("name"), r.PathValue("key")
	path, raw, done, err := s.editConfig()
	defer done()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw.AgentVars[name] != nil {
		delete(raw.AgentVars[name], key)
		if len(raw.AgentVars[name]) == 0 {
			delete(raw.AgentVars, name)
		}
	}
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "var_keys": sortedKeys(raw.AgentVars[name])})
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

// cleanNamesPtr is cleanNames for the tri-state skills field: it preserves the
// difference between nil (unset) and an empty list (explicitly none), which
// cleanNames alone would lose by returning nil for both.
func cleanNamesPtr(in *[]string) *[]string {
	if in == nil {
		return nil
	}
	out := cleanNames(*in)
	if out == nil {
		out = []string{}
	}
	return &out
}
