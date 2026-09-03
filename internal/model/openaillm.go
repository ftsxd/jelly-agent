package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"time"

	openai "github.com/sashabaranov/go-openai"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/telemetry"
)

// ProviderConfig describes one OpenAI-compatible provider endpoint.
type ProviderConfig struct {
	Name    string // logical name, e.g. "deepseek"
	BaseURL string // e.g. https://api.deepseek.com/v1
	APIKey  string
	Model   string // default model id, e.g. deepseek-chat

	// Temperature, when non-nil, overrides the endpoint's default sampling
	// temperature. Note: go-openai tags Temperature `omitempty`, so a literal 0
	// cannot be transmitted and is treated as unset — use a small value such as
	// 0.01 when you want near-deterministic output.
	Temperature *float64
	// MaxTokens caps completion length. Zero ⇒ endpoint default.
	MaxTokens int
	// Timeout bounds time-to-first-byte, NOT the whole exchange, so long
	// streamed answers are never truncated. Zero ⇒ defaultHeaderTimeout.
	Timeout time.Duration
	// MaxRetries bounds retries of transient failures. Nil ⇒ defaultMaxRetries;
	// an explicit 0 disables retrying.
	MaxRetries *int
}

const (
	// defaultHeaderTimeout is generous because a non-streaming request to a
	// reasoning model can take a long while before the first byte arrives.
	defaultHeaderTimeout = 120 * time.Second
	dialTimeout          = 10 * time.Second
)

// OpenAILLM implements google.golang.org/adk/model.LLM on top of an
// OpenAI-compatible HTTP API via go-openai.
type OpenAILLM struct {
	client      *openai.Client
	model       string
	temperature *float64
	maxTokens   int
	maxRetries  int
}

// New builds an OpenAILLM from a ProviderConfig.
func New(cfg ProviderConfig) *OpenAILLM {
	c := openai.DefaultConfig(cfg.APIKey)
	if cfg.BaseURL != "" {
		c.BaseURL = cfg.BaseURL
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultHeaderTimeout
	}
	// ResponseHeaderTimeout rather than http.Client.Timeout: the latter covers
	// reading the body too, which would kill a long SSE stream mid-answer.
	c.HTTPClient = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   dialTimeout,
			ResponseHeaderTimeout: timeout,
		},
	}

	retries := defaultMaxRetries
	if cfg.MaxRetries != nil && *cfg.MaxRetries >= 0 {
		retries = *cfg.MaxRetries
	}
	return &OpenAILLM{
		client:      openai.NewClientWithConfig(c),
		model:       cfg.Model,
		temperature: cfg.Temperature,
		maxTokens:   cfg.MaxTokens,
		maxRetries:  retries,
	}
}

// request assembles the shared chat-completion request, applying the provider's
// generation parameters.
func (m *OpenAILLM) request(req *adkmodel.LLMRequest) openai.ChatCompletionRequest {
	out := openai.ChatCompletionRequest{
		Model:     m.modelID(req),
		Messages:  toOpenAIMessages(req),
		Tools:     toOpenAITools(req),
		MaxTokens: m.maxTokens,
	}
	if m.temperature != nil {
		out.Temperature = float32(*m.temperature)
	}
	return out
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

// generateOnce performs a single blocking completion and yields one response,
// retrying transient failures (429 / 5xx / network) with backoff.
func (m *OpenAILLM) generateOnce(ctx context.Context, req *adkmodel.LLMRequest) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var err error
		for attempt := 0; ; attempt++ {
			var resp openai.ChatCompletionResponse
			resp, err = m.client.CreateChatCompletion(ctx, m.request(req))
			// Retries happen inside the caller's span, so without this the
			// span reads as one slow model call when it was two attempts and
			// a backoff — a wrong answer to "why was this turn slow".
			telemetry.RecordLLMAttempts(ctx, attempt+1)
			if err == nil {
				yield(toLLMResponse(resp), nil)
				return
			}
			if attempt >= m.maxRetries || !retryable(err) {
				break
			}
			if waitErr := waitBackoff(ctx, attempt); waitErr != nil {
				break
			}
		}
		yield(nil, fmt.Errorf("openai chat completion: %w", err))
	}
}

// generateStream streams text deltas as partial responses and folds the rest
// (tool-call fragments, reasoning, usage) via streamAccumulator into a single
// final non-partial response that the runner processes.
// It retries transient failures, but only while nothing has been emitted yet:
// once a delta has reached the caller, replaying the request would duplicate
// text on screen, so a mid-stream failure is surfaced as-is.
func (m *OpenAILLM) generateStream(ctx context.Context, req *adkmodel.LLMRequest) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for attempt := 0; ; attempt++ {
			emitted, err := m.streamAttempt(ctx, req, yield)
			telemetry.RecordLLMAttempts(ctx, attempt+1)
			if err == nil {
				return
			}
			if emitted || attempt >= m.maxRetries || !retryable(err) {
				yield(nil, err)
				return
			}
			if waitErr := waitBackoff(ctx, attempt); waitErr != nil {
				yield(nil, err)
				return
			}
		}
	}
}

// streamAttempt runs one streaming exchange. It reports whether anything was
// handed to the consumer (which makes the attempt unsafe to replay) and the
// error, if any. A nil error means the exchange finished — either successfully
// or because the consumer stopped iterating.
func (m *OpenAILLM) streamAttempt(ctx context.Context, req *adkmodel.LLMRequest, yield func(*adkmodel.LLMResponse, error) bool) (emitted bool, err error) {
	body := m.request(req)
	body.StreamOptions = &openai.StreamOptions{IncludeUsage: true}

	stream, err := m.client.CreateChatCompletionStream(ctx, body)
	if err != nil {
		return false, fmt.Errorf("openai chat stream: %w", err)
	}
	defer stream.Close()

	acc := newStreamAccumulator()
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return emitted, fmt.Errorf("openai stream recv: %w", recvErr)
		}
		if delta := acc.addChunk(chunk); delta != "" {
			partial := &adkmodel.LLMResponse{
				Content: genai.NewContentFromText(delta, genai.RoleModel),
				Partial: true,
			}
			emitted = true
			if !yield(partial, nil) {
				return emitted, nil // consumer stopped; nothing left to do
			}
		}
	}
	yield(acc.final(m.modelID(req)), nil)
	return true, nil
}

// Ensure OpenAILLM satisfies the ADK model interface at compile time.
var _ adkmodel.LLM = (*OpenAILLM)(nil)
