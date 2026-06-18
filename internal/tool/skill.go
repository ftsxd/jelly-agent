package tool

import (
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/jelly-agent/jelly-agent/internal/skill"
)

type useSkillArgs struct {
	Name string `json:"name" jsonschema:"要加载的技能名（取自系统提示「可用技能」清单）"`
}

type useSkillResult struct {
	Found        bool   `json:"found"`
	Name         string `json:"name,omitempty"`
	Description  string `json:"description,omitempty"`
	Instructions string `json:"instructions,omitempty"` // the skill body to follow
	Message      string `json:"message,omitempty"`      // set when not found
}

// SkillTool builds the use_skill tool: given a skill name, it returns that
// skill's full instructions for the agent to follow (progressive disclosure —
// the base prompt only carries the catalog). store must be non-nil.
func SkillTool(store *skill.Store) (adktool.Tool, error) {
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
			return useSkillResult{
				Found: true, Name: sk.Name, Description: sk.Description, Instructions: sk.Body,
			}, nil
		},
	)
}
