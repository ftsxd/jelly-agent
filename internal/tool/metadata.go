package tool

import (
	"time"

	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/toolreg"
)

// BuiltinMetadata describes the tools compiled into this binary.
//
// These entries are in code rather than a YAML file because they describe
// tools that ship with the binary: a built-in whose metadata lived in an
// external file could go missing, and the tool would then be present but
// ungoverned. The overlay in config's metadata_dir layers on top of this for
// MCP servers and for anything that needs tuning without a rebuild.
//
// Name equals the tool's own name for all of these. There is nothing to
// disambiguate — the clash these fields exist to resolve arrives with the
// second MCP server exposing the same verb — and renaming a tool the model
// already knows would cost selection accuracy for no gain.
func BuiltinMetadata() toolreg.Source {
	return toolreg.StaticSource{
		Label: "builtin",
		Metas: []ops.ToolMetadata{
			{
				Name:        "web_search",
				Description: "搜索互联网获取实时信息。需要当前事实、新闻或本地知识以外的内容时使用；已知具体网址时改用 fetch_url。",
				UseCases:    []string{"实时信息", "近期事件", "外部事实核查"},
				AntiExamples: []string{
					"已经知道确切网址时（用 fetch_url）",
					"问题可由已有上下文回答时",
				},
				Produces:       ops.KindText,
				Latency:        ops.LatencySlow, // 出网，且在国内经常超时
				SideEffect:     ops.SideEffectReadOnly,
				Idempotent:     false, // 搜索结果随时间变化
				ParallelSafe:   true,
				Timeout:        20 * time.Second,
				MaxResultBytes: 8000,
			},
			{
				Name:        "fetch_url",
				Description: "抓取指定网址的正文。仅用于已知的公开 http/https 地址；不要用它探测内网。",
				UseCases:    []string{"读取网页正文", "抓取文档页面"},
				AntiExamples: []string{
					"不知道具体网址时（用 web_search）",
					"目标是内网地址时",
				},
				Produces:       ops.KindText,
				Latency:        ops.LatencyMedium,
				SideEffect:     ops.SideEffectReadOnly,
				Idempotent:     true,
				ParallelSafe:   true,
				Timeout:        15 * time.Second,
				MaxResultBytes: 8000,
			},
			{
				Name:        "remember",
				Description: "把值得跨会话记住的事实写入长期记忆。仅用于用户的偏好、身份与重要约定。",
				UseCases:    []string{"记录用户偏好", "记录长期约定"},
				AntiExamples: []string{
					"只在本次对话有用的信息",
					"长期记忆中已有的内容",
				},
				Produces: ops.KindConfig,
				Latency:  ops.LatencyFast,
				// Writes a local file. Reversible and self-inflicted, but not
				// read-only: calling it changes what every later turn sees.
				SideEffect:   ops.SideEffectMutating,
				ParallelSafe: false,
				Timeout:      5 * time.Second,
			},
			{
				Name:         "forget",
				Description:  "从长期记忆中删除一条已过时或用户要求忘记的内容。",
				UseCases:     []string{"信息过时", "用户要求忘记"},
				Produces:     ops.KindConfig,
				Latency:      ops.LatencyFast,
				SideEffect:   ops.SideEffectMutating,
				ParallelSafe: false,
				Timeout:      5 * time.Second,
			},
			{
				Name:        "load_memory",
				Description: "检索过去会话的相关片段。需要回忆此前聊过什么时使用。",
				UseCases:    []string{"回忆历史对话", "查找此前的结论"},
				AntiExamples: []string{
					"答案就在当前对话里时",
				},
				Produces:       ops.KindKnowledge,
				Latency:        ops.LatencyFast,
				SideEffect:     ops.SideEffectReadOnly,
				Idempotent:     true,
				ParallelSafe:   true,
				Fallback:       true, // 便宜且通用，候选集再窄也该留着
				MaxResultBytes: 6000,
			},
			{
				Name:        "use_skill",
				Description: "读取某个技能的完整说明。技能目录里的条目需要展开时使用。",
				UseCases:    []string{"展开技能说明"},
				Produces:    ops.KindKnowledge,
				Latency:     ops.LatencyFast,
				SideEffect:  ops.SideEffectReadOnly,
				Idempotent:  true,
				Fallback:    true,
			},
			{
				Name:        "run_script",
				Description: "在沙箱中运行技能自带的脚本。仅在 use_skill 明确给出脚本时使用。",
				UseCases:    []string{"执行技能脚本"},
				Produces:    ops.KindText,
				Latency:     ops.LatencySlow,
				// Runs code. The sandbox bounds it, but the result of running
				// arbitrary script is not something to classify as harmless.
				SideEffect:     ops.SideEffectRisky,
				ParallelSafe:   false,
				Timeout:        60 * time.Second,
				MaxResultBytes: 8000,
			},
		},
	}
}
