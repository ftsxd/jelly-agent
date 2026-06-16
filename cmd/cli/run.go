package main

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/memory"
	jellymodel "github.com/jelly-agent/jelly-agent/internal/model"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	jellytool "github.com/jelly-agent/jelly-agent/internal/tool"
)

const (
	appName = "jelly-agent"
	userID  = "local-user"

	// rootInstruction is the static base instruction. L1 core memory is
	// prepended to it each turn via the InstructionProvider (PLAN §10.1).
	rootInstruction = "你是 jelly-agent，一个用 Go + ADK-Go 构建的助手。" +
		"需要实时或外部信息时调用 web_search 工具，再用中文简洁作答。" +
		"当用户表达偏好、身份或重要约定，值得跨会话记住时，调用 remember 工具；" +
		"信息过时或用户要求忘记时，调用 forget。不要重复记录上文「长期记忆」中已有的内容。"
)

// buildAgent constructs the root agent for the given provider, returning the
// resolved provider for display and the L2 search service (nil when memory
// search is disabled) for the runner to wire and turns to index into. The
// agent's instruction is generated per turn so it always reflects the latest
// core-memory files.
func buildAgent(reg *jellymodel.Registry, providerName string) (agent.Agent, config.Provider, *memory.Search, error) {
	llm, prov, err := reg.Get(providerName)
	if err != nil {
		return nil, config.Provider{}, nil, err
	}

	core, search, err := loadMemorySetup()
	if err != nil {
		return nil, prov, nil, fmt.Errorf("init memory: %w", err)
	}

	tools, err := jellytool.Builtins(core, search != nil)
	if err != nil {
		return nil, prov, nil, fmt.Errorf("build tools: %w", err)
	}

	a, err := llmagent.New(llmagent.Config{
		Name:        "root",
		Model:       llm,
		Description: "jelly-agent root agent with web search and core memory.",
		// InstructionProvider (not Instruction) so MEMORY.md/USER.md are read
		// fresh each turn. Note: ADK then skips {} session-state substitution.
		InstructionProvider: func(agent.ReadonlyContext) (string, error) {
			return core.Render(rootInstruction), nil
		},
		Tools: tools,
	})
	if err != nil {
		return nil, prov, nil, fmt.Errorf("create agent: %w", err)
	}
	return a, prov, search, nil
}

// newRunner builds a runner backed by the persistent SQLite session store. When
// search is non-nil it is wired as the runner's MemoryService, backing the
// load_memory tool (PLAN §10.1).
func newRunner(a agent.Agent, search *memory.Search) (*runner.Runner, adksession.Service, error) {
	svc, err := jellysession.NewSQLite("")
	if err != nil {
		return nil, nil, err
	}
	cfg := runner.Config{
		AppName:        appName,
		Agent:          a,
		SessionService: svc,
	}
	if search != nil {
		cfg.MemoryService = search
	}
	r, err := runner.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create runner: %w", err)
	}
	return r, svc, nil
}

// runOnce runs a single question in a fresh persisted session, then indexes it
// for future L2 search.
func runOnce(ctx context.Context, a agent.Agent, prov config.Provider, search *memory.Search, sessionID, question string) error {
	if search != nil {
		defer search.Close()
	}
	r, svc, err := newRunner(a, search)
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
