package model

import (
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/history"
)

// This is the test that validates compaction's central premise. internal/history
// reasons about genai contents, but what the provider actually validates is the
// OpenAI message list this package produces: every tool message must follow an
// assistant message whose tool_calls contains its tool_call_id, or the request
// is rejected with a 400. Compaction is only safe if that still holds after it
// has dropped and shortened things.
func assertToolMessagesResolve(t *testing.T, msgs []openai.ChatCompletionMessage) {
	t.Helper()
	announced := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case openai.ChatMessageRoleAssistant:
			for _, tc := range m.ToolCalls {
				announced[tc.ID] = true
			}
		case openai.ChatMessageRoleTool:
			if !announced[m.ToolCallID] {
				t.Errorf("message %d: tool_call_id %q was never announced by a preceding assistant message — the provider would reject this request",
					i, m.ToolCallID)
			}
		}
	}
}

func toolExchange(id string, bodySize int) []*genai.Content {
	return []*genai.Content{
		genai.NewContentFromText("请查一下资料", genai.RoleUser),
		genai.NewContentFromParts([]*genai.Part{{
			FunctionCall: &genai.FunctionCall{ID: id, Name: "fetch_url", Args: map[string]any{"url": "https://example.com"}},
		}}, genai.RoleModel),
		genai.NewContentFromParts([]*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{ID: id, Name: "fetch_url", Response: map[string]any{"content": strings.Repeat("正", bodySize)}},
		}}, genai.RoleUser),
		genai.NewContentFromText("查到了，结论是……", genai.RoleModel),
	}
}

func TestCompactedHistoryStillConvertsToValidMessages(t *testing.T) {
	var contents []*genai.Content
	for i := range 10 {
		contents = append(contents, toolExchange(string(rune('a'+i)), 1500)...)
	}
	contents = append(contents, genai.NewContentFromText("再总结一次", genai.RoleUser))

	// Sanity: the uncompacted history is already valid, so a failure below is
	// compaction's doing rather than a pre-existing conversion bug.
	assertToolMessagesResolve(t, toOpenAIMessages(&adkmodel.LLMRequest{Contents: contents}))

	// Sweep the budget so the drop boundary lands at every position in the
	// history, including exactly between a tool call and its response. A single
	// budget would only prove that one cut happened to be safe.
	droppedSomewhere := false
	for budget := 300; budget <= 30000; budget += 100 {
		compacted, res := history.Compact(contents, history.Policy{
			MaxTokens: budget, KeepRecent: 4, ToolResultTokens: 200,
		})
		if res.Dropped > 0 {
			droppedSomewhere = true
		}
		msgs := toOpenAIMessages(&adkmodel.LLMRequest{Contents: compacted})
		if len(msgs) == 0 {
			t.Fatalf("budget %d: compaction emptied the conversation", budget)
		}
		announced := map[string]bool{}
		for i, m := range msgs {
			switch m.Role {
			case openai.ChatMessageRoleAssistant:
				for _, tc := range m.ToolCalls {
					announced[tc.ID] = true
				}
			case openai.ChatMessageRoleTool:
				if !announced[m.ToolCallID] {
					t.Fatalf("budget %d, message %d: tool_call_id %q was never announced — the provider would reject this request",
						budget, i, m.ToolCallID)
				}
			}
		}
	}
	if !droppedSomewhere {
		t.Fatal("no budget in the sweep triggered a drop; the test never exercised the risky path")
	}
}

// A shortened tool result must still carry its tool_call_id through conversion.
func TestShortenedToolResultKeepsItsID(t *testing.T) {
	contents := toolExchange("call_xyz", 9000)
	compacted, res := history.Compact(contents, history.Policy{
		MaxTokens: 300, KeepRecent: 10, ToolResultTokens: 100,
	})
	if res.Truncated == 0 {
		t.Fatal("expected the oversized tool result to be shortened")
	}

	msgs := toOpenAIMessages(&adkmodel.LLMRequest{Contents: compacted})
	assertToolMessagesResolve(t, msgs)

	var toolMsg *openai.ChatCompletionMessage
	for i := range msgs {
		if msgs[i].Role == openai.ChatMessageRoleTool {
			toolMsg = &msgs[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("tool message disappeared")
	}
	if toolMsg.ToolCallID != "call_xyz" {
		t.Errorf("tool_call_id = %q, want call_xyz", toolMsg.ToolCallID)
	}
	if !strings.Contains(toolMsg.Content, "truncated") {
		t.Errorf("shortened result should say so, got %q", toolMsg.Content)
	}
}
