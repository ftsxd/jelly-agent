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

// handlePrompt reports the fixed part of the prompt and what it costs.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	eng := s.engine()
	parts, err := eng.SystemPrompt(r.URL.Query().Get("provider"))
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

	writeJSON(w, http.StatusOK, map[string]any{
		"parts": out,
		"tools": toolsOut,
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
