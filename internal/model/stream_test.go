package model

import (
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func idxPtr(i int) *int { return &i }

func textChunk(s string) openai.ChatCompletionStreamResponse {
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{Content: s},
		}},
	}
}

func toolChunk(idx int, id, name, args string) openai.ChatCompletionStreamResponse {
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{
				ToolCalls: []openai.ToolCall{{
					Index:    idxPtr(idx),
					ID:       id,
					Type:     openai.ToolTypeFunction,
					Function: openai.FunctionCall{Name: name, Arguments: args},
				}},
			},
		}},
	}
}

// TestStreamAccumulator_ToolCallFragments verifies a single tool call whose
// name and arguments arrive across multiple chunks is reassembled.
func TestStreamAccumulator_ToolCallFragments(t *testing.T) {
	acc := newStreamAccumulator()
	acc.addChunk(toolChunk(0, "call_1", "web_search", `{"que`))
	acc.addChunk(toolChunk(0, "", "", `ry":"go"}`))
	acc.addChunk(openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{FinishReason: openai.FinishReasonToolCalls}},
	})

	final := acc.final("m")
	if len(final.Content.Parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(final.Content.Parts))
	}
	fc := final.Content.Parts[0].FunctionCall
	if fc == nil || fc.ID != "call_1" || fc.Name != "web_search" {
		t.Fatalf("function call = %+v", fc)
	}
	if fc.Args["query"] != "go" {
		t.Errorf("args = %+v, want query=go (fragments not reassembled)", fc.Args)
	}
}

// TestStreamAccumulator_MultipleToolCallsSorted verifies tool calls are emitted
// ordered by index regardless of arrival order.
func TestStreamAccumulator_MultipleToolCallsSorted(t *testing.T) {
	acc := newStreamAccumulator()
	acc.addChunk(toolChunk(1, "call_b", "beta", `{}`))
	acc.addChunk(toolChunk(0, "call_a", "alpha", `{}`))

	final := acc.final("m")
	if len(final.Content.Parts) != 2 {
		t.Fatalf("want 2 parts, got %d", len(final.Content.Parts))
	}
	if final.Content.Parts[0].FunctionCall.Name != "alpha" || final.Content.Parts[1].FunctionCall.Name != "beta" {
		t.Errorf("order = [%s, %s], want [alpha, beta]",
			final.Content.Parts[0].FunctionCall.Name, final.Content.Parts[1].FunctionCall.Name)
	}
}

// TestStreamAccumulator_InvalidJSONArgs verifies malformed arguments are
// surfaced rather than dropped.
func TestStreamAccumulator_InvalidJSONArgs(t *testing.T) {
	acc := newStreamAccumulator()
	acc.addChunk(toolChunk(0, "c", "tool", `not-json`))

	fc := acc.final("m").Content.Parts[0].FunctionCall
	if fc.Args["_raw"] != "not-json" {
		t.Errorf("args = %+v, want _raw passthrough", fc.Args)
	}
}

// TestStreamAccumulator_TextReasoningUsage verifies text deltas accumulate,
// reasoning becomes a leading thought part, and usage/finish are mapped.
func TestStreamAccumulator_TextReasoningUsage(t *testing.T) {
	acc := newStreamAccumulator()
	acc.addChunk(openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{ReasoningContent: "think"},
		}},
	})
	if d := acc.addChunk(textChunk("Hello, ")); d != "Hello, " {
		t.Errorf("addChunk returned %q, want text delta", d)
	}
	acc.addChunk(textChunk("world"))
	acc.addChunk(openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{FinishReason: openai.FinishReasonStop}},
		Usage:   &openai.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	})

	final := acc.final("m")
	if len(final.Content.Parts) != 2 {
		t.Fatalf("want 2 parts (thought+text), got %d", len(final.Content.Parts))
	}
	if !final.Content.Parts[0].Thought || final.Content.Parts[0].Text != "think" {
		t.Errorf("part[0] = %+v, want thought 'think'", final.Content.Parts[0])
	}
	if final.Content.Parts[1].Text != "Hello, world" {
		t.Errorf("text = %q, want 'Hello, world'", final.Content.Parts[1].Text)
	}
	if final.UsageMetadata == nil || final.UsageMetadata.TotalTokenCount != 5 {
		t.Errorf("usage = %+v, want total=5", final.UsageMetadata)
	}
	if !final.TurnComplete {
		t.Error("TurnComplete should be true")
	}
}
