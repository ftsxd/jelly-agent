// Command cli is the Phase 0 minimal entrypoint for jelly-agent: it wires the
// OpenAI-compatible model adapter into an ADK llmagent and runs a single
// question/answer turn through a provider such as DeepSeek.
//
// Usage:
//
//	export LLM_BASE_URL=https://api.deepseek.com/v1
//	export LLM_API_KEY=sk-xxxx
//	export LLM_MODEL=deepseek-chat   # optional, defaults to deepseek-chat
//	go run ./cmd/cli "你好，用一句话介绍你自己"
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
	"google.golang.org/genai"

	jellymodel "github.com/jelly-agent/jelly-agent/internal/model"
)

const (
	appName   = "jelly-agent"
	userID    = "local-user"
	sessionID = "phase0-session"
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

	// 2. A single LLM agent driven by that model.
	a, err := llmagent.New(llmagent.Config{
		Name:        "root_agent",
		Model:       llm,
		Description: "jelly-agent Phase 0 single agent.",
		Instruction: "你是 jelly-agent，一个用 Go + ADK-Go 构建的助手。请简洁回答。",
	})
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}

	// 3. Runner + in-memory session.
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

	// 4. Run one turn and print the model's text.
	fmt.Printf("[ root_agent | %s @ %s ]\nYou: %s\n\nAgent: ", modelID, baseURL, question)

	msg := genai.NewContentFromText(question, genai.RoleUser)
	var answered bool
	for ev, err := range r.Run(ctx, userID, sessionID, msg, agent.RunConfig{StreamingMode: agent.StreamingModeNone}) {
		if err != nil {
			return fmt.Errorf("run: %w", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.Text != "" {
				fmt.Print(p.Text)
				answered = true
			}
		}
		if ev.UsageMetadata != nil {
			u := ev.UsageMetadata
			fmt.Printf("\n\n[Token: prompt=%d completion=%d total=%d]\n",
				u.PromptTokenCount, u.CandidatesTokenCount, u.TotalTokenCount)
		}
	}
	if !answered {
		return fmt.Errorf("no response from model")
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
