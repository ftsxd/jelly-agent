package engine

import (
	"log/slog"
	"slices"
	"strings"

	"google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"

	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/selector"
)

// defaultMaxTools is the cap when config says nothing.
//
// Chosen to be a no-op for any deployment that has not gone looking for this:
// the built-ins are nine, and a couple of MCP servers stay well under it. It
// engages at the point where not cutting is the worse option — around fifty
// tools the schemas alone are several thousand prompt tokens on every turn,
// and a model asked to choose from a catalogue that size gets worse at
// choosing, which costs more than the tokens do.
const defaultMaxTools = 48

// selectingToolset is the single toolset the agent is built with.
//
// Everything funnels through one toolset because the budget is global: two
// servers each offering "not too many" tools is still too many together, and a
// per-server cap cannot see that. It also has to be a toolset rather than the
// static Tools list, because that is the only thing ADK re-consults — it calls
// Tools(ctx) once per invocation and caches the result for that turn
// (internal/llminternal/tools_processor.go). Once per user message with the
// list held stable through the tool-calling loop is exactly the granularity
// selection wants: tools must not appear and vanish mid-loop.
type selectingToolset struct {
	static []adktool.Tool    // built-ins, already bound to the gateway
	sets   []adktool.Toolset // MCP servers, already bound
	cfg    selector.Config
	report func(selector.Result)
	// admit keeps the set stable across a session's turns. Shared across
	// builds, because a toolset instance lives for one request while the
	// prompt cache it protects lives for the conversation. See admit.go.
	admit *admissions
}

func (s *selectingToolset) Name() string { return "jelly_selector" }

func (s *selectingToolset) Tools(ctx agent.ReadonlyContext) ([]adktool.Tool, error) {
	all := slices.Clone(s.static)
	for _, set := range s.sets {
		got, err := set.Tools(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, got...)
	}

	byName := make(map[string]adktool.Tool, len(all))
	order := make(map[string]int, len(all))
	metas := make([]ops.ToolMetadata, 0, len(all))
	for i, t := range all {
		byName[t.Name()] = t
		order[t.Name()] = i
		metas = append(metas, metadataOf(t))
	}

	res := selector.Select(queryOf(ctx), metas, s.cfg)
	if s.report != nil {
		s.report(res)
	}

	// Selected is in catalogue order; the ranking and the matched flag live in
	// Candidates. Both matter here: matched says which slots this question has
	// earned, and the ranking orders the rest.
	var matched, filler []string
	for _, c := range res.Candidates {
		if !slices.Contains(res.Selected, c.Tool) {
			continue
		}
		if c.Matched || c.Baseline {
			matched = append(matched, c.Tool)
		} else {
			filler = append(filler, c.Tool)
		}
	}

	var final []string
	if s.admit != nil {
		final = s.admit.admit(sessionOf(ctx), matched, filler, order, s.cfg.MaxTools)
	} else {
		final = sortByCatalogue(append(append([]string(nil), matched...), filler...), order)
	}

	out := make([]adktool.Tool, 0, len(final))
	for _, name := range final {
		if t, ok := byName[name]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// describedTool is any tool that can state its own metadata.
//
// An interface rather than a check for *gateway.Wrapped: what matters is that
// the tool can describe itself, not which type happens to do so today. It also
// means this path can be exercised without standing up a gateway, which is
// what a test needs to cover the difference between "matched the question" and
// "merely scored above zero".
type describedTool interface {
	Metadata() ops.ToolMetadata
}

// metadataOf recovers what selection ranks against.
//
// A gateway-wrapped tool carries real metadata. Anything else — a tool that
// declined wrapping, or one ADK added itself — is scored on the little it
// does expose, which is enough to rank it and, more to the point, keeps it in
// the running: a tool with no metadata must not be silently unrankable and
// therefore always last.
func metadataOf(t adktool.Tool) ops.ToolMetadata {
	if d, ok := t.(describedTool); ok {
		return d.Metadata()
	}
	return ops.ToolMetadata{Name: t.Name(), Description: t.Description()}
}

// sessionOf identifies the conversation, so the tool set can stay stable
// across its turns. Empty when there is none, which disables the stickiness
// rather than sharing one bucket between unrelated runs.
func sessionOf(ctx agent.ReadonlyContext) string {
	if ctx == nil {
		return ""
	}
	return ctx.SessionID()
}

// queryOf extracts the turn's question.
//
// UserContent is the message that started this invocation, not the latest
// intermediate step — which is what selection wants: the task, not the last
// tool result.
func queryOf(ctx agent.ReadonlyContext) string {
	if ctx == nil {
		return ""
	}
	c := ctx.UserContent()
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			b.WriteString(p.Text)
			b.WriteString(" ")
		}
	}
	return b.String()
}

// logSelection records what reached the model and what did not.
//
// Every candidate, with its score, the fields it matched on, and whether it is
// baseline or fallback — not just the survivors' names and the cut list's
// scores. The question this exists to answer is not "what was dropped" but
// "why did A win over B", and that needs both sides' numbers and the reason
// behind them. An earlier version logged the selected tools as bare names and
// flattened every cut tool's reason to "over budget", which discarded exactly
// the fields that make the comparison possible.
//
// At info when the budget actually removed something, at debug otherwise: a
// selection that changed nothing is not news, but it still has to be
// recoverable when someone asks about a turn after the fact.
func logSelection(res selector.Result) {
	total := len(res.Candidates)
	selected := make(map[string]bool, len(res.Selected))
	for _, n := range res.Selected {
		selected[n] = true
	}

	// Candidates arrive in rank order, so the log reads top-down as the
	// ranking saw it — which is the order the "why did A beat B" question is
	// asked in.
	rows := make([]any, 0, len(res.Candidates))
	for _, c := range res.Candidates {
		attrs := []any{"score", c.Score, "selected", selected[c.Tool]}
		if c.Reason != "" {
			attrs = append(attrs, "matched", c.Reason)
		}
		if c.Baseline {
			attrs = append(attrs, "baseline", true)
		}
		if c.Fallback {
			attrs = append(attrs, "fallback", true)
		}
		if c.Suppressed != "" {
			attrs = append(attrs, "suppressed", c.Suppressed)
		}
		rows = append(rows, slog.Group(c.Tool, attrs...))
	}
	group := slog.Group("candidates", rows...)

	if !res.Capped {
		slog.Debug("工具选择：未裁剪",
			"selected_count", len(res.Selected), "total", total, group)
		return
	}
	slog.Info("工具选择：已按预算裁剪",
		"selected", res.Selected,
		"selected_count", len(res.Selected),
		"total", total,
		"cut_count", total-len(res.Selected),
		group)
}
