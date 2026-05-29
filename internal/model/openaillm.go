package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"

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

// generateStream streams text deltas as partial responses and folds the rest
// (tool-call fragments, reasoning, usage) via streamAccumulator into a single
// final non-partial response that the runner processes.
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

		acc := newStreamAccumulator()
		for {
			chunk, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				yield(nil, fmt.Errorf("openai stream recv: %w", err))
				return
			}
			if delta := acc.addChunk(chunk); delta != "" {
				partial := &adkmodel.LLMResponse{
					Content: genai.NewContentFromText(delta, genai.RoleModel),
					Partial: true,
				}
				if !yield(partial, nil) {
					return
				}
			}
		}
		yield(acc.final(m.modelID(req)), nil)
	}
}

// Ensure OpenAILLM satisfies the ADK model interface at compile time.
var _ adkmodel.LLM = (*OpenAILLM)(nil)
