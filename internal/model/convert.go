// Package model provides an OpenAI-compatible adapter for the ADK-Go model.LLM
// interface. It translates between Google genai types (used by ADK) and the
// OpenAI chat-completions types (used by go-openai), letting any
// OpenAI-compatible provider (DeepSeek / OpenAI / Claude / Ollama ...) drive an
// ADK agent.
package model

import (
	"strings"

	openai "github.com/sashabaranov/go-openai"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// toOpenAIMessages converts an ADK LLMRequest (genai.Content + system
// instruction) into the OpenAI chat message list.
//
// Phase 0 scope: text only. Tool calls / function responses are added in
// Phase 1 together with streaming.
func toOpenAIMessages(req *adkmodel.LLMRequest) []openai.ChatCompletionMessage {
	var msgs []openai.ChatCompletionMessage

	if req.Config != nil && req.Config.SystemInstruction != nil {
		if s := partsText(req.Config.SystemInstruction.Parts); s != "" {
			msgs = append(msgs, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleSystem,
				Content: s,
			})
		}
	}

	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		role := openai.ChatMessageRoleUser
		if c.Role == genai.RoleModel {
			role = openai.ChatMessageRoleAssistant
		}
		msgs = append(msgs, openai.ChatCompletionMessage{
			Role:    role,
			Content: partsText(c.Parts),
		})
	}
	return msgs
}

// partsText concatenates the text of all text parts in a genai content.
func partsText(parts []*genai.Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p != nil && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// toLLMResponse converts a non-streaming OpenAI completion into an ADK
// LLMResponse, mapping the assistant text, finish reason and token usage.
func toLLMResponse(resp openai.ChatCompletionResponse) *adkmodel.LLMResponse {
	out := &adkmodel.LLMResponse{
		Content:      genai.NewContentFromText("", genai.RoleModel),
		ModelVersion: resp.Model,
		TurnComplete: true,
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		out.Content = genai.NewContentFromText(choice.Message.Content, genai.RoleModel)
		out.FinishReason = mapFinishReason(choice.FinishReason)
	}

	out.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     int32(resp.Usage.PromptTokens),
		CandidatesTokenCount: int32(resp.Usage.CompletionTokens),
		TotalTokenCount:      int32(resp.Usage.TotalTokens),
	}
	return out
}

// mapFinishReason maps an OpenAI finish reason to the genai equivalent.
func mapFinishReason(r openai.FinishReason) genai.FinishReason {
	switch r {
	case openai.FinishReasonStop:
		return genai.FinishReasonStop
	case openai.FinishReasonLength:
		return genai.FinishReasonMaxTokens
	default:
		return genai.FinishReasonStop
	}
}
