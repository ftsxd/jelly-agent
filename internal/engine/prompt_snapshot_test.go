package engine

import (
	"testing"

	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	jellytelemetry "github.com/jelly-agent/jelly-agent/internal/telemetry"
)

func TestPromptSnapshotUsesTheSchemasThatReachedTheModel(t *testing.T) {
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("system", genai.RoleUser),
		Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name: "query_instant", Description: "Run PromQL", ParametersJsonSchema: map[string]any{
				"type": "object", "properties": map[string]any{
					"query": map[string]any{"type": "string"},
				},
			},
		}}}},
	}
	sys, tools, _ := jellytelemetry.EstimateConfigTokens(cfg)
	e := New(&config.Config{})
	e.observePrompt("MetricsQuery", cfg, sys, tools)

	snap, ok := e.LastPromptSnapshot("MetricsQuery")
	if !ok || snap.ToolsTokens != tools || len(snap.Tools) != 1 {
		t.Fatalf("snapshot = %+v ok=%v", snap, ok)
	}
	if snap.Tools[0].Name != "query_instant" || snap.Tools[0].Tokens == 0 {
		t.Fatalf("tool snapshot = %+v", snap.Tools[0])
	}
}
