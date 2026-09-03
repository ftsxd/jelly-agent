package telemetry

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/tokens"
)

// Attributes added to ADK's generate_content span.
//
// ADK reports one number for the prompt — gen_ai.usage.input_tokens — which is
// the truth but not an explanation. A one-line question that costs four
// thousand input tokens is normal and also worth understanding, and the split
// below is what turns "4,061" into "3,200 of history, 700 of tool schemas, 150
// of system instruction". Only the history part grows with the conversation, so
// without the split there is no way to tell a long chat from a fat tool set.
//
// These are attributes on ADK's existing span rather than a span of our own:
// wrapping the same call in a second span would duplicate every row in the
// waterfall for no added information.
const (
	attrPromptHistoryTokens = attribute.Key("jelly.prompt.history_tokens")
	attrPromptToolsTokens   = attribute.Key("jelly.prompt.tools_tokens")
	attrPromptSystemTokens  = attribute.Key("jelly.prompt.system_tokens")
	attrPromptTools         = attribute.Key("jelly.prompt.tools")

	// Compaction is otherwise invisible. When a diagnosis rests on evidence
	// gathered eight turns ago, "was that evidence still in the prompt" is the
	// first question, and these are the only place that answers it.
	attrHistoryDropped   = attribute.Key("jelly.history.dropped")
	attrHistoryTruncated = attribute.Key("jelly.history.truncated")
	attrHistoryBefore    = attribute.Key("jelly.history.tokens_before")
	attrHistoryAfter     = attribute.Key("jelly.history.tokens_after")

	// A retried call is one span, so a slow one reads as a slow model when it
	// may have been two attempts and a backoff. Set only when a retry happened,
	// so its presence is the signal.
	attrLLMAttempts = attribute.Key("jelly.llm.attempts")
)

// PromptComposition is the breakdown of one request, as counted before
// compaction ran.
type PromptComposition struct {
	HistoryTokens   int
	ToolsTokens     int
	SystemTokens    int
	ToolCount       int
	DroppedContents int
	TruncatedTools  int
	TokensAfter     int
}

// RecordPrompt annotates the span in ctx — ADK's generate_content span — with
// the composition of the request that produced it, and publishes the same
// numbers as metrics.
//
// Both destinations get the same data for different questions: the span
// explains one turn, the histogram shows the history share creeping up across
// a week. A context without a recording span still records metrics, so
// dashboards keep working when tracing is sampled down.
func RecordPrompt(ctx context.Context, c PromptComposition) {
	recordPromptMetrics(ctx, c)

	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(
		attrPromptHistoryTokens.Int(c.HistoryTokens),
		attrPromptToolsTokens.Int(c.ToolsTokens),
		attrPromptSystemTokens.Int(c.SystemTokens),
		attrPromptTools.Int(c.ToolCount),
		attrHistoryDropped.Int(c.DroppedContents),
		attrHistoryTruncated.Int(c.TruncatedTools),
		attrHistoryBefore.Int(c.HistoryTokens),
		attrHistoryAfter.Int(c.TokensAfter),
	)
}

// RecordLLMAttempts records that a model call took more than one attempt.
//
// One attempt is the normal case and adds nothing, so nothing is written for
// it: an attribute that is present on every span cannot be searched for.
func RecordLLMAttempts(ctx context.Context, attempts int) {
	if attempts <= 1 {
		return
	}
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(attrLLMAttempts.Int(attempts))
}

// EstimateConfigTokens counts the parts of a request that compaction never
// touches: the system instruction and the tool schemas.
//
// Tool schemas are the reason this is worth measuring. They are re-sent on
// every turn, so a wide tool set is a fixed tax on the whole run — commonly
// larger than the evidence it helps gather — and it is invisible in the one
// input-token total the provider reports.
func EstimateConfigTokens(cfg *genai.GenerateContentConfig) (systemTokens, toolsTokens, toolCount int) {
	if cfg == nil {
		return 0, 0, 0
	}
	if cfg.SystemInstruction != nil {
		for _, p := range cfg.SystemInstruction.Parts {
			if p != nil {
				systemTokens += tokens.Estimate(p.Text)
			}
		}
	}
	for _, t := range cfg.Tools {
		if t == nil {
			continue
		}
		for _, fn := range t.FunctionDeclarations {
			if fn == nil {
				continue
			}
			toolCount++
			// The schema reaches the model as JSON, so its serialized size is
			// the honest measure — the declaration's Go representation is not
			// what gets billed.
			toolsTokens += tokens.Estimate(fn.Name) + tokens.Estimate(fn.Description)
			if fn.Parameters != nil {
				if b, err := json.Marshal(fn.Parameters); err == nil {
					toolsTokens += tokens.Estimate(string(b))
				}
			}
		}
	}
	return systemTokens, toolsTokens, toolCount
}
