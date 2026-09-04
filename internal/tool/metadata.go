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
				Name:        "read_result",
				Description: "按 evidence_id 分段读取某次工具调用的完整返回。工具结果里 truncated 为真时，被省略的部分只能从这里取回。",
				UseCases: []string{
					"结果被截断，需要看完整内容",
					"需要返回里被省略的细节",
					"继续读取上一段之后的内容",
				},
				AntiExamples: []string{
					"结果没有被截断时（完整内容已经在上下文里）",
					"想重新执行一次工具时（这里只读已保存的结果，不会重新调用）",
				},
				Produces:     ops.KindText,
				Latency:      ops.LatencyFast,
				SideEffect:   ops.SideEffectReadOnly,
				Idempotent:   true, // 读的是已冻结的结果，不随时间变化
				ParallelSafe: true,
				// Cheap and generally useful, so it stays in the candidate set
				// even when it scores below the cut: a shortlist that drops it
				// leaves the model unable to recover anything it was shown only
				// part of.
				Fallback: true,
				// No ceiling here — the tool pages its own output, bounded by
				// record.MaxWindow, so a second ceiling would cut a window that
				// was already sized to fit.
				MaxResultBytes: 0,
				// A local read of a bounded window, so this is generous rather
				// than tight. It is declared at all because zero means "no
				// deadline" (gateway.runWithTimeout), and a read-only tool with
				// no deadline can only be ended by the caller going away.
				Timeout: 10 * time.Second,
			},
			{
				Name: "search_result",
				// The trigger is "part of it is missing", not "it is large".
				// Measured: the earlier wording said "结果很大时先搜索定位",
				// and on a 42KB result that was already complete in the
				// prompt the model searched it four times and read it three
				// more — for bytes it already had, at four times the tokens.
				Description: "在某次工具调用【被省略的】返回里按正则按行搜索，可带上下文。仅在结果里 truncated 为真时使用；完整内容已在上下文里就直接读上下文。",
				UseCases: []string{
					"结果被截断，要在被省略的部分里定位关键行",
					"统计被省略部分里某类内容出现了多少次",
				},
				AntiExamples: []string{
					"结果没有被截断时（完整内容已经在上下文里，再搜索是白花 token）",
					"想重新执行一次工具时（这里只搜已保存的结果）",
				},
				Produces:     ops.KindText,
				Latency:      ops.LatencyFast,
				SideEffect:   ops.SideEffectReadOnly,
				Idempotent:   true,
				ParallelSafe: true,
				// Paired with read_result: searching to locate and then reading
				// the neighbourhood is the whole point, so a shortlist that
				// keeps one without the other leaves the model reading a large
				// result page by page.
				Fallback:       true,
				MaxResultBytes: 0, // 命中条数由工具自己限制，见 record.MaxHits
				// Longer than read_result, because this one scans: the payload
				// ceiling is sixty-four megabytes and a broad pattern over that
				// much text is real work. Without a declared timeout there is
				// no deadline at all, and the scan's own cancellation check
				// would be the only thing bounding it.
				Timeout: 30 * time.Second,
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
