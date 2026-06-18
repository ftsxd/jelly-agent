package tool

import (
	"context"
	"sort"
	"time"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/jelly-agent/jelly-agent/internal/skill"
)

type useSkillArgs struct {
	Name string `json:"name" jsonschema:"要加载的技能名（取自系统提示「可用技能」清单）"`
}

type useSkillResult struct {
	Found        bool     `json:"found"`
	Name         string   `json:"name,omitempty"`
	Description  string   `json:"description,omitempty"`
	Instructions string   `json:"instructions,omitempty"` // the skill body to follow
	Scripts      []string `json:"scripts,omitempty"`      // runnable script files (call run_script)
	VarKeys      []string `json:"var_keys,omitempty"`     // configured variable names (values injected at run; never shown)
	Message      string   `json:"message,omitempty"`      // set when not found
}

// SkillTool builds the use_skill tool: given a skill name, it returns that
// skill's full instructions for the agent to follow (progressive disclosure —
// the base prompt only carries the catalog). When scriptsEnabled, the result
// also lists the skill's runnable scripts and its configured variable names
// (names only — values are injected into run_script's environment, never shown).
// store must be non-nil; varsFor may be nil.
func SkillTool(store *skill.Store, varsFor func(skill string) map[string]string, scriptsEnabled bool) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name:        "use_skill",
			Description: "按名称加载一个技能的完整步骤说明。当用户的需求匹配系统提示「可用技能」清单中的某项时调用，然后严格按返回的 instructions 执行。",
		},
		func(_ adktool.Context, args useSkillArgs) (useSkillResult, error) {
			sk, ok, err := store.Get(args.Name)
			if err != nil {
				return useSkillResult{}, err
			}
			if !ok || !sk.Enabled {
				return useSkillResult{Found: false, Message: "未找到该技能（或未启用）：" + args.Name}, nil
			}
			res := useSkillResult{Found: true, Name: sk.Name, Description: sk.Description, Instructions: sk.Body}
			if scriptsEnabled {
				res.Scripts = store.Scripts(sk.Name)
				res.VarKeys = sortedVarKeys(varsFor, sk.Name)
			}
			return res, nil
		},
	)
}

type runScriptArgs struct {
	Skill  string   `json:"skill" jsonschema:"技能名"`
	Script string   `json:"script" jsonschema:"要运行的脚本文件名（取自 use_skill 返回的 scripts）"`
	Args   []string `json:"args,omitempty" jsonschema:"传给脚本的命令行参数"`
}

type runScriptResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// runScriptTimeout bounds one script invocation.
const runScriptTimeout = 60 * time.Second

// RunScriptTool builds the run_script tool: it runs a skill's bundled script
// with that skill's configured variables injected as environment (the secret
// values never enter the prompt). Register this ONLY when script execution is
// enabled. store must be non-nil; varsFor may be nil.
func RunScriptTool(store *skill.Store, varsFor func(skill string) map[string]string) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name:        "run_script",
			Description: "运行某个技能附带的脚本（凭据已由系统注入为环境变量，按 use_skill 给出的 var_keys 在脚本里用环境变量引用即可，切勿向用户索要密钥）。返回脚本的输出。",
		},
		func(tc adktool.Context, args runScriptArgs) (runScriptResult, error) {
			ctx, cancel := context.WithTimeout(tc, runScriptTimeout)
			defer cancel()
			var env map[string]string
			if varsFor != nil {
				env = varsFor(args.Skill)
			}
			out, err := store.RunScript(ctx, args.Skill, args.Script, args.Args, env)
			if err != nil {
				return runScriptResult{OK: false, Output: out, Error: err.Error()}, nil
			}
			return runScriptResult{OK: true, Output: out}, nil
		},
	)
}

func sortedVarKeys(varsFor func(string) map[string]string, skill string) []string {
	if varsFor == nil {
		return nil
	}
	m := varsFor(skill)
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
