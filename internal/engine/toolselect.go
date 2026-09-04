package engine

import (
	"log/slog"
	"slices"
	"strings"

	"google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"

	"github.com/jelly-agent/jelly-agent/internal/gateway"
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
	metas := make([]ops.ToolMetadata, 0, len(all))
	for _, t := range all {
		byName[t.Name()] = t
		metas = append(metas, metadataOf(t))
	}

	res := selector.Select(queryOf(ctx), metas, s.cfg)
	if s.report != nil {
		s.report(res)
	}

	out := make([]adktool.Tool, 0, len(res.Selected))
	for _, name := range res.Selected {
		if t, ok := byName[name]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// metadataOf recovers what selection ranks against.
//
// A gateway-wrapped tool carries real metadata. Anything else — a tool that
// declined wrapping, or one ADK added itself — is scored on the little it
// does expose, which is enough to rank it and, more to the point, keeps it in
// the running: a tool with no metadata must not be silently unrankable and
// therefore always last.
func metadataOf(t adktool.Tool) ops.ToolMetadata {
	if w, ok := t.(*gateway.Wrapped); ok {
		return w.Metadata()
	}
	return ops.ToolMetadata{Name: t.Name(), Description: t.Description()}
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
// Logged even when nothing was cut, at debug, because the useful question is
// asked after the fact — "why did it not use X?" — and the answer needs the
// score X got, not just the fact that it was missing. The cut list is the
// difference between an answerable question and a dead end.
func logSelection(res selector.Result) {
	// The total comes from the candidate list, which is the number of tools
	// actually considered. Deriving it at wiring time counted MCP servers
	// rather than the tools they offer, so the log said "3 of 5" about a
	// catalogue of thirty.
	total := len(res.Candidates)
	if !res.Capped {
		slog.Debug("工具选择：未裁剪", "selected", len(res.Selected), "total", total)
		return
	}
	cut := make([]any, 0, 8)
	for _, c := range res.Candidates {
		if c.Suppressed != "" {
			cut = append(cut, slog.Group(c.Tool, "score", c.Score, "reason", c.Suppressed))
		}
	}
	slog.Info("工具选择：已按预算裁剪",
		"selected", res.Selected,
		"selected_count", len(res.Selected),
		"total", total,
		"cut_count", len(cut),
		slog.Group("cut", cut...))
}
