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
			// Project reads deliberately keep Idempotent false: a live grant or
			// snapshot may change between calls, so each must reach the handler.
			{
				Name: "list_code_projects", Description: "列出当前 Agent 获得授权的代码项目和版本，并核对快照是否落后于远端。",
				Suites: []string{"code-analysis"}, Tags: []string{"代码", "项目", "仓库", "分析"},
				Produces: ops.KindText, Latency: ops.LatencyMedium, SideEffect: ops.SideEffectReadOnly,
				// The timeout covers the remote freshness probe, which is a
				// network round trip to the git server. The old 5s ceiling
				// killed the first listing after the probe cache expired — and
				// a listing that dies reads to the model as "没有任何授权项目".
				ParallelSafe: true, Fallback: true, Timeout: 20 * time.Second,
			},
			{
				Name: "read_project_file", Description: "按行读取已授权项目的代码文件，分析具体实现。",
				Suites: []string{"code-analysis"}, Tags: []string{"代码", "文件", "项目", "分析"},
				Produces: ops.KindText, Latency: ops.LatencyFast, SideEffect: ops.SideEffectReadOnly,
				ParallelSafe: true, Timeout: 30 * time.Second,
			},
			{
				Name: "list_project_dir", Description: "查看已授权项目的目录结构和代码文件。",
				Suites: []string{"code-analysis"}, Tags: []string{"代码", "目录", "项目", "分析"},
				Produces: ops.KindText, Latency: ops.LatencyFast, SideEffect: ops.SideEffectReadOnly,
				ParallelSafe: true, Timeout: 5 * time.Second,
			},
			{
				Name: "grep_project_files", Description: "搜索已授权项目的代码，定位函数实现和调用链。",
				Suites: []string{"code-analysis"}, Tags: []string{"代码", "搜索", "项目", "分析"},
				Produces: ops.KindText, Latency: ops.LatencyFast, SideEffect: ops.SideEffectReadOnly,
				ParallelSafe: true, Timeout: 30 * time.Second,
			},
			{
				Name:        "search_project_dirs",
				Description: "按业务名称、描述或标签查找已标注的目录，把业务叫法对应到仓库路径。",
				UseCases:    []string{"用户用业务名称指代服务", "按标签圈定一类服务"},
				AntiExamples: []string{
					"要找的是代码内容而非目录用途时（用 grep_project_files）",
					"已经知道确切目录路径时",
				},
				Suites: []string{"code-analysis"}, Tags: []string{"代码", "目录", "标注", "标签", "项目", "分析"},
				Produces: ops.KindText, Latency: ops.LatencyFast, SideEffect: ops.SideEffectReadOnly,
				ParallelSafe: true, Timeout: 5 * time.Second,
			},
			{
				Name:        "list_project_tags",
				Description: "列出项目里用到的目录标签及各自的目录数量，先看清仓库是怎么分类的。",
				Suites:      []string{"code-analysis"}, Tags: []string{"代码", "标签", "项目", "分析"},
				Produces: ops.KindText, Latency: ops.LatencyFast, SideEffect: ops.SideEffectReadOnly,
				ParallelSafe: true, Timeout: 5 * time.Second,
			},
			{
				// Mutating, not read-only: this is the one project tool that
				// writes. What it writes is a draft nobody has accepted, so it
				// cannot change any analysis — but calling it read-only would
				// put a write behind a label that says there is none.
				Name:        "propose_project_dir_info",
				Description: "为目录提交业务名称、描述和标签的草稿，交用户在代码页面审核采纳。",
				UseCases:    []string{"用户要求给仓库里的服务补上说明", "扫描目录后批量整理业务标注"},
				AntiExamples: []string{
					"用户没有要求整理目录说明时",
					"没有实际读过该目录的代码、只能靠目录名猜测时",
				},
				Suites: []string{"code-analysis"}, Tags: []string{"代码", "目录", "标注", "标签", "项目"},
				Produces: ops.KindText, Latency: ops.LatencyFast, SideEffect: ops.SideEffectMutating,
				Timeout: 15 * time.Second,
			},
			{
				Name:        "check_project_updates",
				Description: "现查远端分支头，确认项目快照是否仍与远端一致。",
				UseCases:    []string{"用户说刚同步过，要确认", "上一次远端核对失败后重试"},
				AntiExamples: []string{
					"list_code_projects 刚给出过 needs_sync 时（那次已经查过，结果一样）",
					"问题与代码版本新旧无关时",
				},
				Suites: []string{"code-analysis"}, Tags: []string{"代码", "项目", "同步", "版本", "更新"},
				Produces: ops.KindText, Latency: ops.LatencyMedium, SideEffect: ops.SideEffectReadOnly,
				ParallelSafe: true, Timeout: 20 * time.Second,
			},
			{
				// The one project tool that changes what every later answer
				// reads: it replaces the snapshot. Risky rather than merely
				// mutating would overstate it — the previous snapshot is a
				// pull away — but it is emphatically not read-only.
				Name:        "sync_project",
				Description: "把项目代码同步到远端最新版本并等待完成；必须先问过用户并得到确认。",
				UseCases: []string{
					"用户在对话里回复确认要同步",
					"用户直接要求更新某个项目的代码",
				},
				AntiExamples: []string{
					"用户还没表态时（先问）",
					"快照已经和远端一致时",
					"用户只是问代码内容、不关心版本新旧时",
				},
				Suites: []string{"code-analysis"}, Tags: []string{"代码", "项目", "同步", "更新", "拉取"},
				Produces: ops.KindText, Latency: ops.LatencySlow, SideEffect: ops.SideEffectMutating,
				// Covers the in-tool wait (3 minutes) plus the pull's own
				// startup. A ceiling under the wait would kill the call at the
				// exact moment the clone is about to land.
				Timeout: 4 * time.Minute,
			},
			{
				// Mutating for the same reason propose_project_dir_info is: it
				// writes a card onto the code page. It still pulls nothing —
				// the person who answers the card does.
				Name:        "request_project_sync",
				Description: "在代码页面留一张同步请求卡片，供用户稍后确认；用于没有用户在对话里的场景。",
				UseCases: []string{
					"定时任务或后台巡检发现快照落后，没人可以当面问",
				},
				AntiExamples: []string{
					"正在和用户对话时（直接问一句，确认后用 sync_project）",
					"快照与远端一致时",
				},
				Suites: []string{"code-analysis"}, Tags: []string{"代码", "项目", "同步", "更新", "请求"},
				Produces: ops.KindText, Latency: ops.LatencyFast, SideEffect: ops.SideEffectMutating,
				Timeout: 15 * time.Second,
			},
			{
				// The history trio reads the object cache the sync already
				// keeps: local disk, no network, no credential. Fast for the
				// same reason grep is, and read-only in the strongest sense —
				// nothing it runs can write to the repository.
				Name:        "log_project_commits",
				Description: "查看已授权项目的提交历史（git log），可按目录、时间、作者、关键词筛选。",
				UseCases: []string{
					"用户问某个服务最近有什么改动",
					"要知道某个目录近期谁在改、改了什么",
				},
				AntiExamples: []string{
					"问的是远端有没有新代码要拉（用 list_code_projects 的 needs_sync）",
					"要看某一段代码现在长什么样（用 read_project_file）",
				},
				Suites: []string{"code-analysis"}, Tags: []string{"代码", "历史", "提交", "git", "项目", "分析"},
				Produces: ops.KindText, Latency: ops.LatencyFast, SideEffect: ops.SideEffectReadOnly,
				ParallelSafe: true, Timeout: 30 * time.Second,
			},
			{
				Name:        "show_project_commit",
				Description: "查看一个提交的作者、时间、提交信息、改动文件，以及可选的代码改动。",
				UseCases: []string{
					"log 里看到一个可疑提交，要看它具体做了什么",
					"要确认某次改动碰了哪些文件",
				},
				AntiExamples: []string{
					"还不知道是哪个提交时（先用 log_project_commits）",
					"要比较两个版本时（用 diff_project_revisions）",
				},
				Suites: []string{"code-analysis"}, Tags: []string{"代码", "历史", "提交", "diff", "git", "项目"},
				Produces: ops.KindText, Latency: ops.LatencyFast, SideEffect: ops.SideEffectReadOnly,
				ParallelSafe: true, Timeout: 30 * time.Second,
			},
			{
				Name:        "diff_project_revisions",
				Description: "比较已授权项目两个提交之间的代码差异，可限定到具体目录或文件。",
				UseCases: []string{
					"排查某次上线前后代码差了什么",
					"确认某个目录在两个版本之间的改动范围",
				},
				AntiExamples: []string{
					"只关心单个提交时（用 show_project_commit）",
					"两个版本里有一个早于本地同步深度时（先调深 history_depth 再同步）",
				},
				Suites: []string{"code-analysis"}, Tags: []string{"代码", "历史", "对比", "diff", "git", "项目"},
				Produces: ops.KindText, Latency: ops.LatencyFast, SideEffect: ops.SideEffectReadOnly,
				ParallelSafe: true, Timeout: 60 * time.Second,
			},
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
				Name: "read_result",
				// The trigger is "what I can see is not enough", not a flag
				// value. A result may be complete and still insufficient, and
				// it may be marked truncated and still answer the question.
				Description: "按 evidence_id 分段读取某次工具调用已保存的完整返回。当前可见信息不足，或需要精确搜索、计数时使用；信息已足够时直接回答。",
				UseCases: []string{
					"进入上下文的只是概况或预览，需要看具体内容",
					"需要返回里被省略的细节",
					"继续读取上一段之后的内容",
				},
				AntiExamples: []string{
					"可见信息已经足够回答时（再读是白花 token）",
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
				Description: "在某次工具调用已保存的完整返回里按正则按行搜索，可带上下文，并给出命中总数。当前可见信息不足，或需要精确搜索、计数时使用；信息已足够时直接回答。",
				UseCases: []string{
					"在超出上下文的返回里定位关键行",
					"统计某类内容出现了多少次（返回全量计数，不必逐段读完）",
				},
				AntiExamples: []string{
					"可见信息已经足够回答时（再搜索是白花 token）",
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
