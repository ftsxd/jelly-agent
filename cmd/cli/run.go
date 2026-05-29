package main

import (
	"context"
	"fmt"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	jellymodel "github.com/jelly-agent/jelly-agent/internal/model"
	jellytool "github.com/jelly-agent/jelly-agent/internal/tool"
)

const appName = "jelly-agent"

// buildAgent constructs the root agent for the given provider, returning the
// resolved provider for display.
func buildAgent(reg *jellymodel.Registry, providerName string) (agent.Agent, config.Provider, error) {
	llm, prov, err := reg.Get(providerName)
	if err != nil {
		return nil, config.Provider{}, err
	}

	tools, err := jellytool.Builtins()
	if err != nil {
		return nil, prov, fmt.Errorf("build tools: %w", err)
	}

	a, err := llmagent.New(llmagent.Config{
		Name:        "root",
		Model:       llm,
		Description: "jelly-agent root agent with web search.",
		Instruction: "你是 jelly-agent，一个用 Go + ADK-Go 构建的助手。" +
			"需要实时或外部信息时调用 web_search 工具，再用中文简洁作答。",
		Tools: tools,
	})
	if err != nil {
		return nil, prov, fmt.Errorf("create agent: %w", err)
	}
	return a, prov, nil
}

// runOnce runs a single question through the agent and streams the answer.
func runOnce(ctx context.Context, a agent.Agent, prov config.Provider, sessionID, question string) error {
	sessions := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        appName,
		Agent:          a,
		SessionService: sessions,
	})
	if err != nil {
		return fmt.Errorf("create runner: %w", err)
	}
	const userID = "local-user"
	if _, err := sessions.Create(ctx, &session.CreateRequest{
		AppName:   appName,
		UserID:    userID,
		SessionID: sessionID,
	}); err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	fmt.Printf("[ root | %s @ %s ]\nYou: %s\n\nAgent: ", prov.Model, prov.BaseURL, question)

	msg := genai.NewContentFromText(question, genai.RoleUser)
	for ev, err := range r.Run(ctx, userID, sessionID, msg, agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) {
		if err != nil {
			return fmt.Errorf("run: %w", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		if ev.Partial {
			for _, p := range ev.Content.Parts {
				if p != nil && !p.Thought && p.Text != "" {
					fmt.Print(p.Text)
				}
			}
			continue
		}
		renderFinal(ev.Content.Parts)
		if ev.UsageMetadata != nil {
			u := ev.UsageMetadata
			fmt.Printf("\n[Token: prompt=%d completion=%d total=%d]\n",
				u.PromptTokenCount, u.CandidatesTokenCount, u.TotalTokenCount)
		}
	}
	return nil
}

// renderFinal surfaces tool calls and results from a non-partial event. Its
// aggregated text is skipped because it was already streamed via partials.
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

func summarize(resp map[string]any) string {
	if results, ok := resp["results"].([]any); ok {
		return fmt.Sprintf("%d 条结果", len(results))
	}
	return fmt.Sprintf("%v", resp)
}
