// Command cli is the Phase 1 entrypoint for jelly-agent: it wires the
// OpenAI-compatible model adapter into an ADK llmagent with a web_search tool
// and runs a single streaming question/answer turn through a provider such as
// DeepSeek, rendering tool-call activity as it happens.
//
// Usage:
//
//	export LLM_BASE_URL=https://api.deepseek.com/v1
//	export LLM_API_KEY=sk-xxxx
//	export LLM_MODEL=deepseek-chat        # optional, defaults to deepseek-chat
//	export TAVILY_API_KEY=tvly-xxx        # optional, better web_search results
//	go run ./cmd/cli "2026 年 Go 在 AI 领域有哪些应用？"
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"

	jellymodel "github.com/jelly-agent/jelly-agent/internal/model"
	jellytool "github.com/jelly-agent/jelly-agent/internal/tool"
)

const (
	appName   = "jelly-agent"
	userID    = "local-user"
	sessionID = "phase1-session"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("LLM_API_KEY is required")
	}
	baseURL := envOr("LLM_BASE_URL", "https://api.deepseek.com/v1")
	modelID := envOr("LLM_MODEL", "deepseek-chat")

	question := strings.TrimSpace(strings.Join(os.Args[1:], " "))
	if question == "" {
		question = "用一句话介绍你自己。"
	}

	// 1. OpenAI-compatible model adapter (the project's foundation).
	llm := jellymodel.New(jellymodel.ProviderConfig{
		Name:    "deepseek",
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   modelID,
	})

	// 2. The web_search tool.
	searchTool, err := jellytool.NewWebSearchTool()
	if err != nil {
		return fmt.Errorf("create web_search tool: %w", err)
	}

	// 3. A single LLM agent with the tool bound.
	a, err := llmagent.New(llmagent.Config{
		Name:        "root_agent",
		Model:       llm,
		Description: "jelly-agent Phase 1 single agent with web search.",
		Instruction: "你是 jelly-agent，一个用 Go + ADK-Go 构建的助手。" +
			"需要实时或外部信息时调用 web_search 工具，再用中文简洁作答。",
		Tools: []adktool.Tool{searchTool},
	})
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}

	// 4. Runner + in-memory session.
	sessions := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        appName,
		Agent:          a,
		SessionService: sessions,
	})
	if err != nil {
		return fmt.Errorf("create runner: %w", err)
	}
	if _, err := sessions.Create(ctx, &session.CreateRequest{
		AppName:   appName,
		UserID:    userID,
		SessionID: sessionID,
	}); err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	// 5. Stream one turn, rendering text deltas and tool-call activity.
	fmt.Printf("[ root_agent | %s @ %s ]\nYou: %s\n\nAgent: ", modelID, baseURL, question)

	msg := genai.NewContentFromText(question, genai.RoleUser)
	for ev, err := range r.Run(ctx, userID, sessionID, msg, agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) {
		if err != nil {
			return fmt.Errorf("run: %w", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		if ev.Partial {
			// Streaming text deltas: print incrementally.
			for _, p := range ev.Content.Parts {
				if p != nil && !p.Thought && p.Text != "" {
					fmt.Print(p.Text)
				}
			}
			continue
		}
		// Final event: surface tool activity. Its aggregated text is skipped
		// because it was already streamed via the partial events above.
		renderFinal(ev.Content.Parts)
		if ev.UsageMetadata != nil {
			u := ev.UsageMetadata
			fmt.Printf("\n[Token: prompt=%d completion=%d total=%d]\n",
				u.PromptTokenCount, u.CandidatesTokenCount, u.TotalTokenCount)
		}
	}
	return nil
}

// renderFinal surfaces tool calls and results from a non-partial event.
func renderFinal(parts []*genai.Part) {
	for _, p := range parts {
		switch {
		case p == nil:
			continue
		case p.FunctionCall != nil:
			fmt.Printf("\n  → 调用工具: %s(%v)\n", p.FunctionCall.Name, p.FunctionCall.Args)
		case p.FunctionResponse != nil:
			fmt.Printf("  → 工具返回: %s\n", summarize(p.FunctionResponse.Response))
		}
	}
}

// summarize renders a tool response compactly for the terminal.
func summarize(resp map[string]any) string {
	if results, ok := resp["results"].([]any); ok {
		return fmt.Sprintf("%d 条结果", len(results))
	}
	return fmt.Sprintf("%v", resp)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
