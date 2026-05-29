package main

import (
	"context"
	"fmt"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	jellymodel "github.com/jelly-agent/jelly-agent/internal/model"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	jellytool "github.com/jelly-agent/jelly-agent/internal/tool"
)

const (
	appName = "jelly-agent"
	userID  = "local-user"
)

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

// newRunner builds a runner backed by the persistent SQLite session store.
func newRunner(a agent.Agent) (*runner.Runner, adksession.Service, error) {
	svc, err := jellysession.NewSQLite("")
	if err != nil {
		return nil, nil, err
	}
	r, err := runner.New(runner.Config{
		AppName:        appName,
		Agent:          a,
		SessionService: svc,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create runner: %w", err)
	}
	return r, svc, nil
}

// runOnce runs a single question in a fresh persisted session.
func runOnce(ctx context.Context, a agent.Agent, prov config.Provider, sessionID, question string) error {
	r, svc, err := newRunner(a)
	if err != nil {
		return err
	}
	if _, err := svc.Create(ctx, &adksession.CreateRequest{AppName: appName, UserID: userID, SessionID: sessionID}); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	fmt.Printf("[ root | %s @ %s | session %s ]\nYou: %s\n\nAgent: ", prov.Model, prov.BaseURL, sessionID, question)
	return runTurn(ctx, r, sessionID, question)
}

// runTurn streams one question/answer turn through the runner.
func runTurn(ctx context.Context, r *runner.Runner, sessionID, question string) error {
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
