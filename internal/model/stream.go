package model

import (
	"sort"
	"strings"

	openai "github.com/sashabaranov/go-openai"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// accumToolCall accumulates one tool call across delta chunks.
type accumToolCall struct {
	id   string
	name string
	args string
}

// streamAccumulator folds an OpenAI streaming response into a single ADK
// response. Tool calls arrive fragmented and keyed by index; text and
// reasoning arrive as deltas. Extracted from the network loop so it can be
// unit-tested with synthetic chunks.
type streamAccumulator struct {
	text      strings.Builder
	reasoning strings.Builder
	toolCalls map[int]*accumToolCall
	usage     *openai.Usage
	finish    openai.FinishReason
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{toolCalls: map[int]*accumToolCall{}}
}

// addChunk folds one stream chunk into the accumulator and returns any new text
// delta (to forward as a partial response). Empty string means no text delta.
func (s *streamAccumulator) addChunk(chunk openai.ChatCompletionStreamResponse) string {
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		return ""
	}
	choice := chunk.Choices[0]
	if choice.FinishReason != "" {
		s.finish = choice.FinishReason
	}
	s.reasoning.WriteString(choice.Delta.ReasoningContent)

	for _, tc := range choice.Delta.ToolCalls {
		idx := 0
		if tc.Index != nil {
			idx = *tc.Index
		}
		acc := s.toolCalls[idx]
		if acc == nil {
			acc = &accumToolCall{}
			s.toolCalls[idx] = acc
		}
		if tc.ID != "" {
			acc.id = tc.ID
		}
		if tc.Function.Name != "" {
			acc.name = tc.Function.Name
		}
		acc.args += tc.Function.Arguments
	}

	if d := choice.Delta.Content; d != "" {
		s.text.WriteString(d)
		return d
	}
	return ""
}

// final assembles the terminal non-partial response (reasoning thought part,
// text, then tool calls ordered by index).
func (s *streamAccumulator) final(model string) *adkmodel.LLMResponse {
	var parts []*genai.Part
	if s.reasoning.Len() > 0 {
		// Preserved so it can be echoed back to thinking models on the
		// follow-up request that carries the tool results.
		parts = append(parts, &genai.Part{Text: s.reasoning.String(), Thought: true})
	}
	if s.text.Len() > 0 {
		parts = append(parts, genai.NewPartFromText(s.text.String()))
	}

	indices := make([]int, 0, len(s.toolCalls))
	for idx := range s.toolCalls {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		tc := s.toolCalls[idx]
		if tc.name == "" {
			continue
		}
		parts = append(parts, &genai.Part{
			FunctionCall: &genai.FunctionCall{
				ID:   tc.id,
				Name: tc.name,
				Args: parseArgs(tc.args),
			},
		})
	}

	return &adkmodel.LLMResponse{
		Content:       genai.NewContentFromParts(parts, genai.RoleModel),
		ModelVersion:  model,
		UsageMetadata: toUsage(s.usage),
		FinishReason:  mapFinishReason(s.finish),
		TurnComplete:  true,
	}
}
