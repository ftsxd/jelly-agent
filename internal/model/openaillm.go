package model

import (
	"context"
	"fmt"
	"iter"

	openai "github.com/sashabaranov/go-openai"
	adkmodel "google.golang.org/adk/model"
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

// GenerateContent implements model.LLM.
//
// Phase 0: non-streaming only. When stream is requested we still perform a
// single blocking call and yield one complete response — real token streaming
// (and tool-call delta accumulation) lands in Phase 1.
func (m *OpenAILLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		model := m.model
		if req.Model != "" {
			model = req.Model
		}

		resp, err := m.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model:    model,
			Messages: toOpenAIMessages(req),
		})
		if err != nil {
			yield(nil, fmt.Errorf("openai chat completion: %w", err))
			return
		}
		yield(toLLMResponse(resp), nil)
	}
}

// Ensure OpenAILLM satisfies the ADK model interface at compile time.
var _ adkmodel.LLM = (*OpenAILLM)(nil)
