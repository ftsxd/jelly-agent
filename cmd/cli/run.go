package main

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/memory"
)

// runOnce runs a single question in a fresh persisted session, then indexes it
// for future L2 search.
func runOnce(ctx context.Context, eng *engine.Engine, a agent.Agent, prov config.Provider, search *memory.Search, sessionID, question string) error {
	if search != nil {
		defer search.Close()
	}
	r, svc, err := eng.NewRunner(a, search)
	if err != nil {
		return err
	}
	if _, err := svc.Create(ctx, &adksession.CreateRequest{AppName: appName, UserID: userID, SessionID: sessionID}); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	fmt.Printf("[ root | %s @ %s | session %s ]\nYou: %s\n\nAgent: ", prov.Model, prov.BaseURL, sessionID, question)
	if err := runTurn(ctx, r, sessionID, question); err != nil {
		return err
	}
	indexSession(ctx, svc, search, sessionID)
	return nil
}

// indexSession ingests the named session into the L2 search index so later
// sessions can retrieve it. It is a no-op when search is disabled; indexing
// failures are surfaced as warnings without aborting the turn.
func indexSession(ctx context.Context, svc adksession.Service, search *memory.Search, sessionID string) {
	if search == nil {
		return
	}
	resp, err := svc.Get(ctx, &adksession.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil || resp.Session == nil {
		fmt.Fprintf(os.Stderr, "\n[记忆索引警告] 无法读取会话: %v\n", err)
		return
	}
	if err := search.AddSessionToMemory(ctx, resp.Session); err != nil {
		fmt.Fprintf(os.Stderr, "\n[记忆索引警告] %v\n", err)
	}
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
