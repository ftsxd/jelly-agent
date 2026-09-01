package server

import (
	"errors"
	"testing"

	"google.golang.org/genai"
)

func TestToolError(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		want string
	}{
		{"nil response", nil, ""},
		{"no error key", map[string]any{"results": []any{}}, ""},
		{"nil error value", map[string]any{"error": nil}, ""},
		{"empty error counts as success", map[string]any{"error": ""}, ""},
		{"string error", map[string]any{"error": "duckduckgo request: timeout"}, "duckduckgo request: timeout"},
		{"error value", map[string]any{"error": errors.New("boom")}, "boom"},
		{"non-string error", map[string]any{"error": 42}, "42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolError(tc.resp); got != tc.want {
				t.Fatalf("toolError = %q, want %q", got, tc.want)
			}
			if got, want := toolFailed(tc.resp), tc.want != ""; got != want {
				t.Fatalf("toolFailed = %v, want %v", got, want)
			}
		})
	}
}

// A failed tool reaches the transcript through the ordinary FunctionResponse
// channel, so the DTO must mark it failed rather than merely "returned".
func TestEventDTOMarksFailedToolResult(t *testing.T) {
	ev := &genai.Content{Parts: []*genai.Part{
		{FunctionResponse: &genai.FunctionResponse{
			Name:     "web_search",
			Response: map[string]any{"error": "duckduckgo request: context deadline exceeded"},
		}},
		{FunctionResponse: &genai.FunctionResponse{
			Name:     "web_search",
			Response: map[string]any{"results": []any{"a", "b"}},
		}},
	}}

	var got []toolResult
	for _, p := range ev.Parts {
		resp := p.FunctionResponse.Response
		got = append(got, toolResult{
			Name: p.FunctionResponse.Name, Response: resp,
			OK: !toolFailed(resp), Error: toolError(resp),
		})
	}

	if got[0].OK {
		t.Fatal("errored tool response reported as OK")
	}
	if got[0].Error == "" {
		t.Fatal("errored tool response carries no message")
	}
	if !got[1].OK || got[1].Error != "" {
		t.Fatalf("successful tool response marked failed: %+v", got[1])
	}
}
