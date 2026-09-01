package history

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

func textContent(role genai.Role, s string) *genai.Content {
	return genai.NewContentFromText(s, role)
}

func callContent(id, name string, argSize int) *genai.Content {
	return genai.NewContentFromParts([]*genai.Part{{
		FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: map[string]any{"q": strings.Repeat("x", argSize)}},
	}}, genai.RoleModel)
}

func respContent(id, name string, bodySize int) *genai.Content {
	return genai.NewContentFromParts([]*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: id, Name: name, Response: map[string]any{"content": strings.Repeat("字", bodySize)}},
	}}, genai.RoleUser)
}

// callIDs / responseIDs collect the tool-call and tool-response ids present in
// a history, which is what the pairing invariant is checked against.
func callIDs(contents []*genai.Content) map[string]bool {
	out := map[string]bool{}
	for _, c := range contents {
		for _, p := range c.Parts {
			if p != nil && p.FunctionCall != nil {
				out[p.FunctionCall.ID] = true
			}
		}
	}
	return out
}

func responseIDs(contents []*genai.Content) map[string]bool {
	out := map[string]bool{}
	for _, c := range contents {
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil {
				out[p.FunctionResponse.ID] = true
			}
		}
	}
	return out
}

// assertPaired is the invariant that matters most: convert.go maps a
// FunctionResponse to a tool message with tool_call_id, which the provider
// rejects unless the matching tool_calls message is also present.
func assertPaired(t *testing.T, contents []*genai.Content) {
	t.Helper()
	calls, resps := callIDs(contents), responseIDs(contents)
	for id := range resps {
		if !calls[id] {
			t.Errorf("orphan tool response %q: its tool_call is missing, the provider would reject this request", id)
		}
	}
}

func TestCompactLeavesSmallHistoryAlone(t *testing.T) {
	in := []*genai.Content{
		textContent(genai.RoleUser, "你好"),
		textContent(genai.RoleModel, "你好，有什么可以帮你？"),
	}
	out, res := Compact(in, Policy{MaxTokens: 10000})
	if res.Changed() {
		t.Errorf("small history was compacted: %+v", res)
	}
	if len(out) != len(in) {
		t.Errorf("len = %d, want %d", len(out), len(in))
	}
}

func TestCompactShortensToolResultsBeforeDropping(t *testing.T) {
	in := []*genai.Content{
		textContent(genai.RoleUser, "查一下"),
		callContent("c1", "fetch_url", 10),
		respContent("c1", "fetch_url", 6000), // ~6000 tokens of CJK
		textContent(genai.RoleModel, "查到了"),
		textContent(genai.RoleUser, "再说说"),
	}
	out, res := Compact(in, Policy{MaxTokens: 2000, KeepRecent: 2, ToolResultTokens: 300})

	if res.Truncated == 0 {
		t.Error("expected a tool result to be shortened")
	}
	if res.Dropped != 0 {
		t.Errorf("dropped %d contents, but shortening should have sufficed", res.Dropped)
	}
	if res.AfterTokens > 2000 {
		t.Errorf("after = %d tokens, still over budget 2000", res.AfterTokens)
	}
	assertPaired(t, out)
}

// The whole point of grouping: a call and its response are dropped together.
//
// A single budget only proves that one particular cut was safe — the dangerous
// cuts are the ones that land exactly between a call and its response. Sweeping
// budgets walks the drop boundary across every position in the history, so an
// implementation that splits a pair cannot slip through on lucky arithmetic.
func TestCompactNeverOrphansToolResponses(t *testing.T) {
	// The call content is deliberately bulky: dropping it frees enough tokens
	// that the loop can finish right there, which is precisely the moment a
	// naive implementation would leave the response behind unpaired.
	var in []*genai.Content
	for i := range 12 {
		in = append(in,
			textContent(genai.RoleUser, "问题"),
			callContent(string(rune('a'+i)), "fetch_url", 1600),
			respContent(string(rune('a'+i)), "fetch_url", 1200),
			textContent(genai.RoleModel, "回答"),
		)
	}

	droppedSomewhere := false
	for budget := 200; budget <= 20000; budget += 100 {
		out, res := Compact(in, Policy{MaxTokens: budget, KeepRecent: 4, ToolResultTokens: 200})
		if res.Dropped > 0 {
			droppedSomewhere = true
		}
		if res.AfterTokens > res.BeforeTokens {
			t.Fatalf("budget %d: compaction grew the history: %d → %d", budget, res.BeforeTokens, res.AfterTokens)
		}
		calls, resps := callIDs(out), responseIDs(out)
		for id := range resps {
			if !calls[id] {
				t.Fatalf("budget %d: orphan tool response %q — the provider would reject this request", budget, id)
			}
		}
	}
	if !droppedSomewhere {
		t.Fatal("no budget in the sweep triggered a drop; the test never exercised the risky path")
	}
}

func TestCompactKeepsRecentTail(t *testing.T) {
	var in []*genai.Content
	for range 20 {
		in = append(in, textContent(genai.RoleUser, strings.Repeat("长", 400)))
	}
	last := textContent(genai.RoleUser, "这是最新的问题")
	in = append(in, last)

	out, res := Compact(in, Policy{MaxTokens: 500, KeepRecent: 3})
	if res.Dropped == 0 {
		t.Fatal("expected drops")
	}
	if out[len(out)-1] != last {
		t.Error("the newest message was dropped; it must always reach the model")
	}
}

// ADK reuses these objects for session state and the live stream, so a previous
// bug class in this repo is "compaction rewrote the caller's data".
func TestCompactDoesNotMutateInput(t *testing.T) {
	in := []*genai.Content{
		textContent(genai.RoleUser, "问题"),
		callContent("c1", "fetch_url", 10),
		respContent("c1", "fetch_url", 8000),
		textContent(genai.RoleUser, "最新"),
	}
	originalBody := in[2].Parts[0].FunctionResponse.Response["content"].(string)
	originalLen := len(in)

	out, res := Compact(in, Policy{MaxTokens: 500, KeepRecent: 1, ToolResultTokens: 100})
	if !res.Changed() {
		t.Fatal("expected compaction to do something")
	}
	if len(in) != originalLen {
		t.Errorf("input slice length changed: %d → %d", originalLen, len(in))
	}
	if got := in[2].Parts[0].FunctionResponse.Response["content"].(string); got != originalBody {
		t.Error("input tool response was rewritten in place")
	}
	if len(out) > 0 && out[0] == in[0] && res.Dropped > 0 {
		t.Error("dropped contents but the output still starts with the original first content")
	}
}

func TestCompactAddsPlaceholderWhenDropping(t *testing.T) {
	var in []*genai.Content
	for range 30 {
		in = append(in, textContent(genai.RoleUser, strings.Repeat("话", 200)))
	}
	out, res := Compact(in, Policy{MaxTokens: 400, KeepRecent: 2})
	if res.Dropped == 0 {
		t.Fatal("expected drops")
	}
	first := out[0].Parts[0].Text
	if !strings.Contains(first, "已省略") {
		t.Errorf("no placeholder explaining the omission, got %q", first)
	}
}

// L2 session search is off by default, so load_memory usually does not exist.
// Advertising it anyway invites the model to hallucinate a call to it.
func TestPlaceholderOnlyMentionsLoadMemoryWhenAvailable(t *testing.T) {
	var in []*genai.Content
	for range 30 {
		in = append(in, textContent(genai.RoleUser, strings.Repeat("话", 200)))
	}

	withoutRecall, res := Compact(in, Policy{MaxTokens: 400, KeepRecent: 2})
	if res.Dropped == 0 {
		t.Fatal("expected drops")
	}
	if got := withoutRecall[0].Parts[0].Text; strings.Contains(got, "load_memory") {
		t.Errorf("suggested load_memory although the agent has no such tool: %q", got)
	}

	withRecall, res := Compact(in, Policy{MaxTokens: 400, KeepRecent: 2, CanRecall: true})
	if res.Dropped == 0 {
		t.Fatal("expected drops")
	}
	if got := withRecall[0].Parts[0].Text; !strings.Contains(got, "load_memory") {
		t.Errorf("load_memory is available but was not offered: %q", got)
	}
}

// The tail alone can exceed the budget; compaction must then shorten tool
// results inside it rather than give up or drop the current question.
func TestCompactShortensProtectedTailAsLastResort(t *testing.T) {
	in := []*genai.Content{
		callContent("c1", "fetch_url", 10),
		respContent("c1", "fetch_url", 9000),
		textContent(genai.RoleUser, "总结一下"),
	}
	out, res := Compact(in, Policy{MaxTokens: 500, KeepRecent: 5, ToolResultTokens: 200})
	if res.Truncated == 0 {
		t.Error("expected the protected tail's tool result to be shortened")
	}
	if res.Dropped != 0 {
		t.Errorf("protected tail was dropped (%d contents)", res.Dropped)
	}
	assertPaired(t, out)
	if len(out) != len(in) {
		t.Errorf("len = %d, want %d (nothing should be removed)", len(out), len(in))
	}
}

func TestShortenedResponseKeepsToolCallID(t *testing.T) {
	in := []*genai.Content{
		callContent("call_abc", "fetch_url", 10),
		respContent("call_abc", "fetch_url", 9000),
		textContent(genai.RoleUser, "继续"),
	}
	out, _ := Compact(in, Policy{MaxTokens: 400, KeepRecent: 5, ToolResultTokens: 150})

	var found bool
	for _, c := range out {
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil {
				found = true
				if p.FunctionResponse.ID != "call_abc" {
					t.Errorf("tool_call_id = %q, want call_abc", p.FunctionResponse.ID)
				}
				if p.FunctionResponse.Name != "fetch_url" {
					t.Errorf("name = %q, want fetch_url", p.FunctionResponse.Name)
				}
				if p.FunctionResponse.Response["truncated"] != true {
					t.Errorf("shortened response not marked truncated: %+v", p.FunctionResponse.Response)
				}
			}
		}
	}
	if !found {
		t.Fatal("tool response vanished entirely")
	}
}

func TestBuildGroupsBindsCallToResponses(t *testing.T) {
	in := []*genai.Content{
		textContent(genai.RoleUser, "问"),
		callContent("c1", "a", 5),
		respContent("c1", "a", 5),
		respContent("c1b", "b", 5), // parallel call answered in a second content
		textContent(genai.RoleModel, "答"),
	}
	groups := buildGroups(in)
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3 (text | call+2 responses | text)", len(groups))
	}
	if len(groups[1].items) != 3 {
		t.Errorf("call group holds %d contents, want 3", len(groups[1].items))
	}
}

func TestEstimateContentsCountsToolPayloads(t *testing.T) {
	small := []*genai.Content{textContent(genai.RoleUser, "hi")}
	big := []*genai.Content{respContent("c1", "fetch_url", 1000)}
	if EstimateContents(big) <= EstimateContents(small) {
		t.Error("tool payloads must count toward the estimate")
	}
}
