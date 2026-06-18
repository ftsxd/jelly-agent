package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/agent"
	adksession "google.golang.org/adk/session"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/memory"
)

// runInteractive starts a multi-turn REPL on a single persisted session.
// Inline commands (prefixed with /) control the session; Ctrl+D exits. When
// search is non-nil each turn is indexed into L2 memory for later retrieval.
func runInteractive(ctx context.Context, eng *engine.Engine, a agent.Agent, prov config.Provider, search *memory.Search) error {
	if search != nil {
		defer search.Close()
	}
	r, svc, err := eng.NewRunner(a, search)
	if err != nil {
		return err
	}

	sessionID := newSessionID()
	if _, err := svc.Create(ctx, &adksession.CreateRequest{AppName: appName, UserID: userID, SessionID: sessionID}); err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	fmt.Printf("jelly-agent CLI  [ root | %s @ %s ]\n", prov.Model, prov.BaseURL)
	fmt.Printf("session: %s   输入 /help 查看命令，/exit 或 Ctrl+D 退出\n", sessionID)
	fmt.Println(strings.Repeat("─", 60))

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Print("\nYou: ")
		if !scanner.Scan() {
			fmt.Println("\n再见。")
			return nil // EOF (Ctrl+D)
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			newID, quit := handleInlineCommand(ctx, svc, line, sessionID)
			if quit {
				return nil
			}
			if newID != "" {
				sessionID = newID
			}
			continue
		}

		fmt.Print("\nAgent: ")
		if err := runTurn(ctx, r, sessionID, line); err != nil {
			fmt.Fprintf(os.Stderr, "\n[错误] %v\n", err)
			continue
		}
		indexSession(ctx, svc, search, sessionID)
	}
}

// handleInlineCommand processes a /command. It returns a new session id (when
// /clear starts a fresh session) and whether to quit.
func handleInlineCommand(ctx context.Context, svc adksession.Service, line, sessionID string) (newID string, quit bool) {
	cmd := strings.Fields(line)[0]
	switch cmd {
	case "/exit", "/quit":
		fmt.Println("再见。")
		return "", true
	case "/help":
		fmt.Println(`可用命令：
  /tools          列出当前可用工具
  /memory         显示长期记忆（MEMORY.md / USER.md）
  /clear          清空上下文（开新会话）
  /stats          显示当前会话 token 用量
  /help           显示本帮助
  /exit           退出（等价 Ctrl+D）
  （/model、/agent 切换将在后续批次提供）`)
	case "/memory":
		printMemory()
	case "/tools":
		tools, err := listBuiltins()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[错误] %v\n", err)
			return "", false
		}
		for _, t := range tools {
			fmt.Printf("  - %s: %s\n", t.Name(), t.Description())
		}
	case "/clear":
		fresh := newSessionID()
		if _, err := svc.Create(ctx, &adksession.CreateRequest{AppName: appName, UserID: userID, SessionID: fresh}); err != nil {
			fmt.Fprintf(os.Stderr, "[错误] %v\n", err)
			return "", false
		}
		fmt.Printf("已开新会话：%s\n", fresh)
		return fresh, false
	case "/stats":
		printSessionStats(ctx, svc, sessionID)
	default:
		fmt.Printf("未知命令 %q，输入 /help 查看可用命令。\n", cmd)
	}
	return "", false
}

// printSessionStats sums token usage across the session's events.
func printSessionStats(ctx context.Context, svc adksession.Service, sessionID string) {
	resp, err := svc.Get(ctx, &adksession.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil || resp.Session == nil {
		fmt.Fprintf(os.Stderr, "[错误] 无法读取会话: %v\n", err)
		return
	}
	var prompt, completion, total int32
	for ev := range resp.Session.Events().All() {
		if ev.UsageMetadata != nil {
			prompt += ev.UsageMetadata.PromptTokenCount
			completion += ev.UsageMetadata.CandidatesTokenCount
			total += ev.UsageMetadata.TotalTokenCount
		}
	}
	fmt.Printf("会话 %s：prompt=%d completion=%d total=%d\n", sessionID, prompt, completion, total)
}

// printMemory shows the current L1 core-memory files.
func printMemory() {
	core, err := loadMemory()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[错误] %v\n", err)
		return
	}
	mem, usr := core.Snapshot()
	fmt.Printf("记忆目录: %s\n", core.Dir())
	if usr == "" && mem == "" {
		fmt.Println("（暂无长期记忆）")
		return
	}
	if usr != "" {
		fmt.Println("\n[USER.md]")
		fmt.Println(usr)
	}
	if mem != "" {
		fmt.Println("\n[MEMORY.md]")
		fmt.Println(mem)
	}
}

func newSessionID() string {
	return fmt.Sprintf("cli-%d", time.Now().UnixNano())
}
