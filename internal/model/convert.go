// Package model provides an OpenAI-compatible adapter for the ADK-Go model.LLM
// interface. It translates between Google genai types (used by ADK) and the
// OpenAI chat-completions types (used by go-openai), letting any
// OpenAI-compatible provider (DeepSeek / OpenAI / Claude / Ollama ...) drive an
// ADK agent.
package model

import (
	"encoding/json"
	"strings"

	openai "github.com/sashabaranov/go-openai"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// toOpenAIMessages converts an ADK LLMRequest into the OpenAI chat message
// list. Each genai.Content is dispatched by the kind of parts it holds:
//
//   - text parts            -> user/assistant text message
//   - FunctionCall parts    -> assistant message carrying tool_calls
//   - FunctionResponse parts -> one tool message per response (tool_call_id)
//
// Dispatching on part type (rather than the content Role) keeps the mapping
// robust regardless of how ADK labels history entries.
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

		var (
			text      strings.Builder
			toolCalls []openai.ToolCall
			responses []openai.ChatCompletionMessage
		)
		for _, p := range c.Parts {
			switch {
			case p == nil:
				continue
			case p.FunctionCall != nil:
				toolCalls = append(toolCalls, openai.ToolCall{
					ID:   p.FunctionCall.ID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      p.FunctionCall.Name,
						Arguments: marshalArgs(p.FunctionCall.Args),
					},
				})
			case p.FunctionResponse != nil:
				responses = append(responses, openai.ChatCompletionMessage{
					Role:       openai.ChatMessageRoleTool,
					ToolCallID: p.FunctionResponse.ID,
					Content:    marshalArgs(p.FunctionResponse.Response),
				})
			case p.Text != "":
				text.WriteString(p.Text)
			}
		}

		switch {
		case len(toolCalls) > 0:
			msgs = append(msgs, openai.ChatCompletionMessage{
				Role:      openai.ChatMessageRoleAssistant,
				Content:   text.String(),
				ToolCalls: toolCalls,
			})
		case len(responses) > 0:
			msgs = append(msgs, responses...)
		case text.Len() > 0:
			role := openai.ChatMessageRoleUser
			if c.Role == genai.RoleModel {
				role = openai.ChatMessageRoleAssistant
			}
			msgs = append(msgs, openai.ChatCompletionMessage{Role: role, Content: text.String()})
		}
	}
	return msgs
}

// toOpenAITools converts the function declarations ADK placed on the request
// into OpenAI tool definitions.
func toOpenAITools(req *adkmodel.LLMRequest) []openai.Tool {
	if req.Config == nil {
		return nil
	}
	var tools []openai.Tool
	for _, gt := range req.Config.Tools {
		if gt == nil {
			continue
		}
		for _, decl := range gt.FunctionDeclarations {
			if decl == nil {
				continue
			}
			tools = append(tools, openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        decl.Name,
					Description: decl.Description,
					Parameters:  toolParameters(decl),
				},
			})
		}
	}
	return tools
}

// toolParameters extracts a JSON-schema object for the tool parameters,
// preferring the JSON-schema form that functiontool emits, then the genai
// Schema, and finally an empty object so providers that require a schema are
// satisfied.
func toolParameters(decl *genai.FunctionDeclaration) any {
	if decl.ParametersJsonSchema != nil {
		return decl.ParametersJsonSchema
	}
	if decl.Parameters != nil {
		return decl.Parameters
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
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

// marshalArgs renders a key/value map as a JSON object string, returning "{}"
// for empty or unmarshalable input.
func marshalArgs(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// parseArgs parses a tool_call arguments JSON string into a map.
func parseArgs(s string) map[string]any {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		// Surface the raw payload so the tool can still inspect it.
		return map[string]any{"_raw": s}
	}
	return m
}

// toLLMResponse converts a non-streaming OpenAI completion into an ADK
// LLMResponse, mapping assistant text, tool calls, finish reason and usage.
func toLLMResponse(resp openai.ChatCompletionResponse) *adkmodel.LLMResponse {
	out := &adkmodel.LLMResponse{
		ModelVersion: resp.Model,
		TurnComplete: true,
	}

	var parts []*genai.Part
	if len(resp.Choices) > 0 {
		msg := resp.Choices[0].Message
		if msg.Content != "" {
			parts = append(parts, genai.NewPartFromText(msg.Content))
		}
		for _, tc := range msg.ToolCalls {
			parts = append(parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   tc.ID,
					Name: tc.Function.Name,
					Args: parseArgs(tc.Function.Arguments),
				},
			})
		}
		out.FinishReason = mapFinishReason(resp.Choices[0].FinishReason)
	}
	out.Content = genai.NewContentFromParts(parts, genai.RoleModel)
	out.UsageMetadata = toUsage(&resp.Usage)
	return out
}

// toUsage maps OpenAI token usage onto the genai usage metadata.
func toUsage(u *openai.Usage) *genai.GenerateContentResponseUsageMetadata {
	if u == nil {
		return nil
	}
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     int32(u.PromptTokens),
		CandidatesTokenCount: int32(u.CompletionTokens),
		TotalTokenCount:      int32(u.TotalTokens),
	}
}

// mapFinishReason maps an OpenAI finish reason to the genai equivalent.
func mapFinishReason(r openai.FinishReason) genai.FinishReason {
	switch r {
	case openai.FinishReasonStop:
		return genai.FinishReasonStop
	case openai.FinishReasonLength:
		return genai.FinishReasonMaxTokens
	default:
		// tool_calls / function_call / empty all map to STOP for ADK's purposes;
		// the presence of FunctionCall parts is what drives the tool loop.
		return genai.FinishReasonStop
	}
}
