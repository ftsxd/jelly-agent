package tool

import (
	"fmt"
	"strings"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/jelly-agent/jelly-agent/internal/memory"
)

// rememberArgs / forgetArgs are the parameters the model fills when editing
// core memory. The `jsonschema` tags become property descriptions in the tool
// schema sent to the model.
type rememberArgs struct {
	Fact   string `json:"fact" jsonschema:"要长期记住的一条事实，简洁一句话"`
	Target string `json:"target,omitempty" jsonschema:"写入哪份记忆：memory 代理长期笔记（默认），或 user 用户画像"`
}

type rememberResult struct {
	Stored bool   `json:"stored"`
	Target string `json:"target"`
}

type forgetArgs struct {
	Match  string `json:"match" jsonschema:"要删除的记忆所含关键词（不区分大小写子串匹配）"`
	Target string `json:"target,omitempty" jsonschema:"从哪份记忆删除：memory(默认) 或 user"`
}

type forgetResult struct {
	Removed int    `json:"removed"`
	Target  string `json:"target"`
}

// MemoryTools builds the remember/forget tools backed by core. They let the
// agent persist or drop long-term facts in MEMORY.md / USER.md across sessions
// (PLAN §10.4). core must be non-nil.
func MemoryTools(core *memory.Core) ([]adktool.Tool, error) {
	remember, err := functiontool.New(
		functiontool.Config{
			Name:        "remember",
			Description: "把一条值得长期记住的事实写入核心记忆（跨会话保留）。用户表达偏好、身份或重要约定时调用。",
		},
		func(_ adktool.Context, args rememberArgs) (rememberResult, error) {
			target, err := parseTarget(args.Target)
			if err != nil {
				return rememberResult{}, err
			}
			if err := core.Remember(target, args.Fact); err != nil {
				return rememberResult{}, err
			}
			return rememberResult{Stored: true, Target: string(target)}, nil
		},
	)
	if err != nil {
		return nil, err
	}

	forget, err := functiontool.New(
		functiontool.Config{
			Name:        "forget",
			Description: "从核心记忆中删除包含指定关键词的条目。用户要求忘记某事或信息已过时时调用。",
		},
		func(_ adktool.Context, args forgetArgs) (forgetResult, error) {
			target, err := parseTarget(args.Target)
			if err != nil {
				return forgetResult{}, err
			}
			n, err := core.Forget(target, args.Match)
			if err != nil {
				return forgetResult{}, err
			}
			return forgetResult{Removed: n, Target: string(target)}, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return []adktool.Tool{remember, forget}, nil
}

func parseTarget(s string) (memory.Target, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "memory":
		return memory.TargetMemory, nil
	case "user":
		return memory.TargetUser, nil
	default:
		return "", fmt.Errorf("未知 target %q（应为 memory 或 user）", s)
	}
}
