package model

import (
	"testing"

	openai "github.com/sashabaranov/go-openai"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// TestToOpenAIMessages_TextRoundTrip checks system instruction + user/model
// text are mapped to the right OpenAI roles.
func TestToOpenAIMessages_TextRoundTrip(t *testing.T) {
	req := &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("be brief", genai.RoleUser),
		},
		Contents: []*genai.Content{
			genai.NewContentFromText("hello", genai.RoleUser),
			genai.NewContentFromText("hi there", genai.RoleModel),
		},
	}

	msgs := toOpenAIMessages(req)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(msgs), msgs)
	}
	want := []struct{ role, content string }{
		{openai.ChatMessageRoleSystem, "be brief"},
		{openai.ChatMessageRoleUser, "hello"},
		{openai.ChatMessageRoleAssistant, "hi there"},
	}
	for i, w := range want {
		if msgs[i].Role != w.role || msgs[i].Content != w.content {
			t.Errorf("msg[%d] = {%s, %q}, want {%s, %q}", i, msgs[i].Role, msgs[i].Content, w.role, w.content)
		}
	}
}

// TestToOpenAIMessages_ToolCallRoundTrip checks an assistant FunctionCall and a
// following FunctionResponse map to a tool_calls assistant message and a tool
// message with the matching tool_call_id.
func TestToOpenAIMessages_ToolCallRoundTrip(t *testing.T) {
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{
			genai.NewContentFromParts([]*genai.Part{{
				FunctionCall: &genai.FunctionCall{ID: "call_1", Name: "web_search", Args: map[string]any{"query": "go"}},
			}}, genai.RoleModel),
			genai.NewContentFromParts([]*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{ID: "call_1", Name: "web_search", Response: map[string]any{"ok": true}},
			}}, genai.RoleUser),
		},
	}

	msgs := toOpenAIMessages(req)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d: %+v", len(msgs), msgs)
	}

	asst := msgs[0]
	if asst.Role != openai.ChatMessageRoleAssistant || len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant message malformed: %+v", asst)
	}
	if got := asst.ToolCalls[0]; got.ID != "call_1" || got.Function.Name != "web_search" {
		t.Errorf("tool call = %+v, want id=call_1 name=web_search", got)
	}
	if asst.ToolCalls[0].Function.Arguments != `{"query":"go"}` {
		t.Errorf("tool call args = %q", asst.ToolCalls[0].Function.Arguments)
	}

	toolMsg := msgs[1]
	if toolMsg.Role != openai.ChatMessageRoleTool || toolMsg.ToolCallID != "call_1" {
		t.Errorf("tool message = %+v, want role=tool tool_call_id=call_1", toolMsg)
	}
}

// TestReasoningRoundTrip checks that a thinking model's reasoning_content is
// preserved on the assistant turn carrying tool_calls (DeepSeek requirement),
// and dropped from a plain completed turn.
func TestReasoningRoundTrip(t *testing.T) {
	withCall := &adkmodel.LLMRequest{
		Contents: []*genai.Content{
			genai.NewContentFromParts([]*genai.Part{
				{Text: "let me search", Thought: true},
				{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "web_search", Args: map[string]any{"q": "x"}}},
			}, genai.RoleModel),
		},
	}
	msgs := toOpenAIMessages(withCall)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if msgs[0].ReasoningContent != "let me search" || len(msgs[0].ToolCalls) != 1 {
		t.Errorf("assistant msg = %+v, want reasoning echoed with tool call", msgs[0])
	}

	// A thought on a plain text turn (no tool call) must NOT carry reasoning back.
	plain := &adkmodel.LLMRequest{
		Contents: []*genai.Content{
			genai.NewContentFromParts([]*genai.Part{
				{Text: "thinking...", Thought: true},
				{Text: "final answer"},
			}, genai.RoleModel),
		},
	}
	pm := toOpenAIMessages(plain)
	if len(pm) != 1 || pm[0].Content != "final answer" || pm[0].ReasoningContent != "" {
		t.Errorf("plain turn = %+v, want only content with no reasoning", pm[0])
	}
}

// TestToLLMResponse_ToolCalls checks a completion carrying tool_calls becomes
// genai FunctionCall parts with parsed args.
func TestToLLMResponse_ToolCalls(t *testing.T) {
	resp := openai.ChatCompletionResponse{
		Model: "deepseek-chat",
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				ToolCalls: []openai.ToolCall{{
					ID:       "call_9",
					Type:     openai.ToolTypeFunction,
					Function: openai.FunctionCall{Name: "web_search", Arguments: `{"query":"adk-go"}`},
				}},
			},
			FinishReason: openai.FinishReasonToolCalls,
		}},
		Usage: openai.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}

	out := toLLMResponse(resp)
	if out.Content == nil || len(out.Content.Parts) != 1 {
		t.Fatalf("want 1 part, got %+v", out.Content)
	}
	fc := out.Content.Parts[0].FunctionCall
	if fc == nil || fc.ID != "call_9" || fc.Name != "web_search" {
		t.Fatalf("function call = %+v", fc)
	}
	if fc.Args["query"] != "adk-go" {
		t.Errorf("args = %+v, want query=adk-go", fc.Args)
	}
	if out.UsageMetadata == nil || out.UsageMetadata.TotalTokenCount != 15 {
		t.Errorf("usage = %+v, want total=15", out.UsageMetadata)
	}
}

// TestToOpenAITools checks function declarations become OpenAI tools.
func TestToOpenAITools(t *testing.T) {
	req := &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{
				FunctionDeclarations: []*genai.FunctionDeclaration{{
					Name:        "web_search",
					Description: "search the web",
				}},
			}},
		},
	}

	tools := toOpenAITools(req)
	if len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	if tools[0].Type != openai.ToolTypeFunction || tools[0].Function.Name != "web_search" {
		t.Errorf("tool = %+v", tools[0])
	}
}

// TestToolParameters_GenaiSchemaLowercased guards the load_memory regression:
// a tool declared with the genai Schema form (uppercase "OBJECT"/"STRING") must
// be emitted as lowercase JSON-Schema types, else providers reject it with
// "STRING is not valid under any of the schemas in 'anyOf'".
func TestToolParameters_GenaiSchemaLowercased(t *testing.T) {
	decl := &genai.FunctionDeclaration{
		Name: "load_memory",
		Parameters: &genai.Schema{
			Type: "OBJECT",
			Properties: map[string]*genai.Schema{
				"query": {Type: "STRING", Description: "the query"},
			},
			Required: []string{"query"},
		},
	}

	params, ok := toolParameters(decl).(map[string]any)
	if !ok {
		t.Fatalf("want map params, got %T", toolParameters(decl))
	}
	if params["type"] != "object" {
		t.Errorf("top type = %v, want object", params["type"])
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("want properties map, got %T", params["properties"])
	}
	q, ok := props["query"].(map[string]any)
	if !ok {
		t.Fatalf("want query schema map, got %T", props["query"])
	}
	if q["type"] != "string" {
		t.Errorf("query type = %v, want string", q["type"])
	}
}
