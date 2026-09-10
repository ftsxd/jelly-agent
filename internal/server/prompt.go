package server

// What we inject into every turn.
//
// The fixed part of a prompt — the instruction, core memory, the skill
// catalogue, and every selected tool's schema — is re-sent on each model call,
// so it is a tax on the whole run rather than a one-off. It is also invisible
// in the single input-token figure a provider reports, which is why a run can
// cost far more than its conversation appears to justify and nobody can say
// where it went.
//
// The composition already goes to traces and metrics (telemetry.RecordPrompt),
// but those answer "is the history share creeping up this week", not "what did
// this deployment just send". This endpoint answers the second question, and it
// gets the text from Engine.SystemPrompt so there is one assembly of it rather
// than a handler's opinion about one.

import (
	"net/http"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/config"

	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/tokens"
)

type promptPartDTO struct {
	Name      string `json:"name"`
	Text      string `json:"text"`
	Tokens    int    `json:"tokens"`
	Assembled bool   `json:"assembled,omitempty"`
}

type promptToolDTO struct {
	Name        string `json:"name"`
	Server      string `json:"server,omitempty"`
	Description string `json:"description"`
	SideEffect  string `json:"side_effect,omitempty"`
	Tokens      int    `json:"tokens"`
	// Bound is the tool's result ceiling. It is here because it is the field
	// most likely to explain a surprising bill: a ceiling that cuts a JSON
	// result in half leaves the model unable to use it, and a model that
	// cannot use a result asks again — each retry re-sending the whole
	// history.
	Bound int `json:"max_result_bytes,omitempty"`
}

// handlePrompt reports the per-call prompt overhead and its estimated cost.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	eng := s.engineFor(r)
	// Which agent's prompt. Empty falls back to the configured default, so the
	// page opens on what a turn would actually use rather than on the built-in
	// base that a deployment with a coordinator never sends.
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		agent = eng.Config().DefaultAgent
	}
	parts, err := eng.SystemPrompt(r.URL.Query().Get("provider"), agent)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]promptPartDTO, 0, len(parts))
	systemTokens := 0
	for _, p := range parts {
		n := tokens.Estimate(p.Text)
		if p.Assembled {
			// The assembled text is the sum of the ingredients plus the glue,
			// so it is the one to report as the total and the one a reader
			// must not add to the others.
			systemTokens = n
		}
		out = append(out, promptPartDTO{Name: p.Name, Text: p.Text, Tokens: n, Assembled: p.Assembled})
	}

	registry := eng.ToolRegistry()
	metas := registry.Available(nil)
	toolsOut := make([]promptToolDTO, 0, len(metas))
	toolsTokens := 0
	for _, m := range metas {
		// The declaration is what actually costs tokens, but the registry holds
		// metadata rather than schemas, so this estimates from the text the
		// model reads: name, description, use cases and anti-examples.
		text := m.Name + m.Description
		for _, u := range m.UseCases {
			text += u
		}
		for _, a := range m.AntiExamples {
			text += a
		}
		n := tokens.Estimate(text)
		toolsTokens += n
		toolsOut = append(toolsOut, promptToolDTO{
			Name: m.Name, Server: m.Server, Description: m.Description,
			SideEffect: string(effectiveEffect(m)), Tokens: n, Bound: m.MaxResultBytes,
		})
	}
	// Prefer what the model actually received. Metadata cannot reconstruct an
	// MCP declaration's parameter schema and does not contain undeclared MCP
	// tools at all, so the old estimate commonly under-reported this fixed cost
	// by several times. Before the first model call we still return the labelled
	// fallback above; afterwards the input shape is an exact model-boundary
	// observation while the token count remains a tokenizer estimate.
	measured := false
	measuredAt := int64(0)
	snapshotAgent := agent
	if snapshotAgent == "" {
		snapshotAgent = "root"
	}
	if snap, ok := eng.LastPromptSnapshot(snapshotAgent); ok {
		measured = true
		measuredAt = snap.At.UnixMilli()
		toolsTokens = snap.ToolsTokens
		toolsOut = make([]promptToolDTO, 0, len(snap.Tools))
		for _, t := range snap.Tools {
			row := promptToolDTO{
				Name: t.Name, Description: t.Description,
				Tokens: t.Tokens, Server: t.Server,
			}
			// The level always resolves, declared or not. A lookup miss is
			// every MCP tool nobody has declared — 39 of 42 here — and
			// leaving the field empty rendered a blank badge, which reads as
			// "no side effect". Policy says the opposite: a remote tool that
			// says nothing is assumed to mutate.
			m, known := registry.Lookup(t.Name)
			if known {
				row.Bound = m.MaxResultBytes
			} else {
				m = ops.ToolMetadata{Name: t.Name, Server: t.Server}
			}
			row.SideEffect = string(effectiveEffect(m))
			toolsOut = append(toolsOut, row)
		}
	}

	// Which agent this is the prompt for, and whether the text shown is that
	// agent's own or the base it inherits. The page offers to edit it, and
	// those two edit different things.
	agents := make([]string, 0, len(eng.Config().Agents))
	for _, a := range eng.Config().Agents {
		agents = append(agents, a.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"parts":          out,
		"tools":          toolsOut,
		"agent":          agent,
		"agents":         agents,
		"own":            agent != "" && eng.InstructionFor(agent) != eng.BaseInstruction(),
		"base_editable":  true,
		"tools_measured": measured,
		// When, so the page can say measured *when*. Zero while unmeasured.
		"tools_measured_at": measuredAt,
		"totals": map[string]int{
			"system_tokens":  systemTokens,
			"tools_tokens":   toolsTokens,
			"tools":          len(toolsOut),
			"fixed_tokens":   systemTokens + toolsTokens,
			"max_tools":      eng.MaxTools(),
			"history_budget": historyBudget(eng.Config()),
		},
	})
}

// effectiveEffect resolves what the gateway will actually treat a tool as.
//
// An undeclared level is not "unknown" to the policy: a built-in that says
// nothing is read-only, while a remote one that says nothing is assumed to
// mutate, because a third-party server's silence is not a safety guarantee.
// Showing the raw empty string would misreport the tool as harmless.
func effectiveEffect(m ops.ToolMetadata) ops.SideEffectLevel {
	if m.SideEffect != "" {
		return m.SideEffect
	}
	if m.Server != "" {
		return ops.SideEffectMutating
	}
	return ops.SideEffectReadOnly
}

// historyBudget resolves the configured history ceiling. It is a pointer in
// config so that "unset" and "zero" stay distinct; nil means the engine's own
// default applies, which this view reports as 0 rather than inventing a number
// it would have to keep in sync.
func historyBudget(cfg *config.Config) int {
	if cfg == nil || cfg.History.MaxTokens == nil {
		return 0
	}
	return *cfg.History.MaxTokens
}

// promptInput sets the base system instruction.
type promptInput struct {
	Instruction string `json:"instruction"`
}

// handleSaveInstruction rewrites the base system instruction in config.
//
// The base one, not an agent's: an agent's lives on the agent and is edited on
// the Agent page. This is the text every agent starts from and the only one a
// single-agent deployment ever uses — the built-in describes a general
// assistant, and a deployment that is an ops-diagnosis agent has to be able to
// say so without rebuilding a binary.
//
// An empty value clears the override and restores the built-in default, which
// is the only way back once something has been written.
func (s *Server) handleSaveInstruction(w http.ResponseWriter, r *http.Request) {
	var in promptInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
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
	raw.Instruction = strings.TrimSpace(in.Instruction)
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "saved_to": path, "instruction": s.engineAfterReload().BaseInstruction(),
	})
}
