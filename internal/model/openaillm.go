package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"sort"

	openai "github.com/sashabaranov/go-openai"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// ProviderConfig describes one OpenAI-compatible provider endpoint.
type ProviderConfig struct {
	Name    string // logical name, e.g. "deepseek"
	BaseURL string // e.g. https://api.deepseek.com/v1
	APIKey  string
	Model   string // default model id, e.g. deepseek-chat
}

// OpenAILLM implements google.golang.org/adk/model.LLM on top of an
// OpenAI-compatible HTTP API via go-openai.
type OpenAILLM struct {
	client *openai.Client
	model  string
}

// New builds an OpenAILLM from a ProviderConfig.
func New(cfg ProviderConfig) *OpenAILLM {
	c := openai.DefaultConfig(cfg.APIKey)
	if cfg.BaseURL != "" {
		c.BaseURL = cfg.BaseURL
	}
	return &OpenAILLM{
		client: openai.NewClientWithConfig(c),
		model:  cfg.Model,
	}
}

// Name implements model.LLM.
func (m *OpenAILLM) Name() string { return m.model }

// GenerateContent implements model.LLM, dispatching to the streaming or
// blocking path. Both translate tool declarations into OpenAI tools and emit
// genai FunctionCall parts so the ADK runner can drive the tool-call loop.
func (m *OpenAILLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	if stream {
		return m.generateStream(ctx, req)
	}
	return m.generateOnce(ctx, req)
}

func (m *OpenAILLM) modelID(req *adkmodel.LLMRequest) string {
	if req.Model != "" {
		return req.Model
	}
	return m.model
}

// generateOnce performs a single blocking completion and yields one response.
func (m *OpenAILLM) generateOnce(ctx context.Context, req *adkmodel.LLMRequest) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		resp, err := m.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model:    m.modelID(req),
			Messages: toOpenAIMessages(req),
			Tools:    toOpenAITools(req),
		})
		if err != nil {
			yield(nil, fmt.Errorf("openai chat completion: %w", err))
			return
		}
		yield(toLLMResponse(resp), nil)
	}
}

// accumToolCall accumulates a streamed tool call across delta chunks.
type accumToolCall struct {
	id   string
	name string
	args string
}

// generateStream streams text deltas as partial responses and accumulates
// tool-call deltas (which arrive fragmented, keyed by index) into complete
// function calls, emitting a single final non-partial response that the runner
// processes.
func (m *OpenAILLM) generateStream(ctx context.Context, req *adkmodel.LLMRequest) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		stream, err := m.client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
			Model:         m.modelID(req),
			Messages:      toOpenAIMessages(req),
			Tools:         toOpenAITools(req),
			StreamOptions: &openai.StreamOptions{IncludeUsage: true},
		})
		if err != nil {
			yield(nil, fmt.Errorf("openai chat stream: %w", err))
			return
		}
		defer stream.Close()

		var (
			fullText  string
			toolCalls = map[int]*accumToolCall{}
			usage     *openai.Usage
			finish    openai.FinishReason
		)

		for {
			chunk, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				yield(nil, fmt.Errorf("openai stream recv: %w", err))
				return
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			choice := chunk.Choices[0]
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}

			if d := choice.Delta.Content; d != "" {
				fullText += d
				partial := &adkmodel.LLMResponse{
					Content: genai.NewContentFromText(d, genai.RoleModel),
					Partial: true,
				}
				if !yield(partial, nil) {
					return
				}
			}

			for _, tc := range choice.Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				acc := toolCalls[idx]
				if acc == nil {
					acc = &accumToolCall{}
					toolCalls[idx] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				acc.args += tc.Function.Arguments
			}
		}

		yield(m.finalStreamResponse(req, fullText, toolCalls, usage, finish), nil)
	}
}

// finalStreamResponse assembles the terminal, non-partial response from the
// accumulated text and tool calls.
func (m *OpenAILLM) finalStreamResponse(req *adkmodel.LLMRequest, text string, toolCalls map[int]*accumToolCall, usage *openai.Usage, finish openai.FinishReason) *adkmodel.LLMResponse {
	var parts []*genai.Part
	if text != "" {
		parts = append(parts, genai.NewPartFromText(text))
	}

	indices := make([]int, 0, len(toolCalls))
	for idx := range toolCalls {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		tc := toolCalls[idx]
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
		ModelVersion:  m.modelID(req),
		UsageMetadata: toUsage(usage),
		FinishReason:  mapFinishReason(finish),
		TurnComplete:  true,
	}
}

// Ensure OpenAILLM satisfies the ADK model interface at compile time.
var _ adkmodel.LLM = (*OpenAILLM)(nil)
