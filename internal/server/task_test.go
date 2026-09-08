package server

import (
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// infos stands in for the tool registry. An entry means the tool declared what
// it is; no entry means it did not, which is what every MCP tool does — the
// adopted metadata carries no Produces and no side-effect level.
func infos(m map[string]ToolInfo) func(string) ToolInfo {
	return func(tool string) ToolInfo { return m[tool] }
}

func known(kind ops.EvidenceKind) ToolInfo { return ToolInfo{Kind: kind, Known: true} }

func fr(typ string, kv ...any) map[string]any {
	f := map[string]any{"type": typ}
	for i := 0; i+1 < len(kv); i += 2 {
		f[kv[i].(string)] = kv[i+1]
	}
	return f
}

func call(round, callID, name string, ts int64, args ...any) map[string]any {
	a := map[string]any{}
	for i := 0; i+1 < len(args); i += 2 {
		a[args[i].(string)] = args[i+1]
	}
	return fr(frameToolCall, "round", round, "call_id", callID, "name", name,
		"ts", ts, "agent", "root", "args", a)
}

func result(round, callID string, ok bool, ts int64, resp map[string]any) map[string]any {
	return fr(frameToolResult, "round", round, "call_id", callID, "ok", ok, "ts", ts, "response", resp)
}

// answer is a terminal assistant message; narrate is the running commentary a
// model emits alongside a tool call. Both are top-level assistant text, and
// telling them apart is what makes the difference between quoting the reply
// and quoting a progress line.
func answer(round, text string, ts int64) map[string]any {
	return fr(frameText, "round", round, "text", text, "ts", ts, "final", true)
}

func narrate(round, text string, ts int64) map[string]any {
	return fr(frameText, "round", round, "text", text, "ts", ts, "final", false)
}

// 验收 2：一次监控查询调用 query_instant 和 query_range，只出现一个「查询监控」
// 步骤，详情里两个工具调用都在。
//
// The shape is copied from a real session: the model narrates before each
// call, which is exactly what used to split the two into separate phases.
func TestTwoWaysOfAskingOneSystemAreOneStep(t *testing.T) {
	// Declared the way configs/tools/n9e.yaml declares them. An MCP server
	// reports a name and a description and nothing about what it produces, so
	// this is config the deployment owns — see the file for why.
	k := infos(map[string]ToolInfo{
		"query_instant": known(ops.KindMetricSeries),
		"query_range":   known(ops.KindMetricSeries),
	})
	frames := []map[string]any{
		fr(frameUserMessage, "text", "看下 redis 的 CPU", "ts", int64(1)),
		narrate("r1", "你说得对，我确实有 PromQL 查询工具。现在直接查：", 10),
		call("r1", "c1", "query_instant", 11, "query", "redis_cpu"),
		result("r1", "c1", true, 20, map[string]any{"summary": "2 个实例", "evidence_id": "e1"}),
		narrate("r1", "找到数据源了。现在拉近 24h 的曲线：", 30),
		call("r1", "c2", "query_range", 31, "query", "redis_cpu[24h]"),
		result("r1", "c2", true, 40, map[string]any{"summary": "两条曲线", "evidence_id": "e2"}),
		answer("r1", "spp 下 2 个腾讯云 Redis 实例近 24h 的 CPU 使用率如下…", 50),
	}

	got := foldTasks("web-1", frames, k, nil)[0]
	if len(got.Steps) != 2 {
		t.Fatalf("steps = %v, want one query step and the conclusion", labels(got))
	}
	if got.Steps[0].Calls != 2 || len(got.Steps[0].Tools) != 2 {
		t.Errorf("the query step holds %d calls: %+v", got.Steps[0].Calls, got.Steps[0].Tools)
	}
	if got.Steps[0].Tools[0].Name != "query_instant" || got.Steps[0].Tools[1].Name != "query_range" {
		t.Errorf("tools = %+v", got.Steps[0].Tools)
	}
	// The step is named for what it was for, not for the tool that did it.
	if got.Steps[0].Label != "查询监控" {
		t.Errorf("label = %q; the tool name must stay in the detail", got.Steps[0].Label)
	}
	// 验收 5：the reply is the terminal message, not the narration.
	if got.Reply != "spp 下 2 个腾讯云 Redis 实例近 24h 的 CPU 使用率如下…" {
		t.Errorf("reply = %q", got.Reply)
	}
	// The narration is kept where it belongs: as what the model said it was
	// doing during that step.
	if got.Steps[0].Note == "" {
		t.Error("the step lost the model's own account of what it was doing")
	}
}

// 验收 1：a run that only talked is not a task.
func TestPlainConversationIsNotATask(t *testing.T) {
	k := infos(nil)
	for _, text := range []string{"你好", "hello", "你是谁"} {
		got := foldTasks("web-1", []map[string]any{
			fr(frameUserMessage, "text", text, "ts", int64(1)),
			answer("r1", "你好！我是运维助手。", 10),
		}, k, nil)[0]

		if !got.Chat {
			t.Errorf("%q was not recognised as conversation", text)
		}
		if IsTask(got, noRecords) {
			t.Errorf("%q reached the task centre", text)
		}
	}
}

// The same run becomes a task the moment something is actually done.
func TestARunThatDidSomethingIsATask(t *testing.T) {
	k := infos(nil)
	got := foldTasks("web-1", []map[string]any{
		fr(frameUserMessage, "text", "查一下告警", "ts", int64(1)),
		call("r1", "c1", "list_alert_rules", 10),
		result("r1", "c1", true, 20, map[string]any{"summary": "18 条"}),
		answer("r1", "共 18 条告警规则。", 30),
	}, k, nil)[0]

	if got.Chat || !IsTask(got, noRecords) {
		t.Errorf("a run with a tool call was treated as conversation: %+v", got)
	}
}

// A run that stored something is a task even when the fold cannot see a tool
// call — which is the shape of older data, and of a run whose events are
// thinner than the store's record of it.
func TestARunThatStoredSomethingIsATask(t *testing.T) {
	got := foldTasks("web-1", []map[string]any{
		fr(frameUserMessage, "text", "x", "ts", int64(1)),
		answer("r1", "好的。", 10),
	}, infos(nil), nil)[0]

	if IsTask(got, noRecords) {
		t.Fatal("precondition: it looks like conversation on its own")
	}
	if !IsTask(got, someRecords) {
		t.Error("a run with a stored result was excluded")
	}
}

// A scheduled run is always a task: the scheduler asked for it, so somebody
// wants to know how it went even if it did nothing.
func TestAScheduledRunIsAlwaysATask(t *testing.T) {
	got := foldTasks("schedule-nightly", []map[string]any{
		answer("r1", "巡检完成，无异常。", 10),
	}, infos(nil), nil)[0]

	if !IsTask(got, noRecords) {
		t.Error("a scheduled run was excluded")
	}
	if got.Type != TypeInspection {
		t.Errorf("type = %q, want inspection", got.Type)
	}
}

// Phases, not tools: fetching is one purpose, reading back what was fetched is
// another, and changing something is a third.
func TestStepsFollowPurposeNotTool(t *testing.T) {
	k := infos(map[string]ToolInfo{
		"get_metrics":   known(ops.KindMetricSeries),
		"get_logs":      known(ops.KindLogExcerpt),
		"restart_pod":   {Known: true, Mutating: true},
		"search_result": known(ops.KindText),
		"read_result":   known(ops.KindText),
	})
	frames := []map[string]any{
		fr(frameUserMessage, "text", "排查", "ts", int64(1)),
		call("r1", "c1", "get_metrics", 10),
		result("r1", "c1", true, 11, map[string]any{"summary": "ok", "evidence_id": "e1"}),
		call("r1", "c2", "get_logs", 20),
		result("r1", "c2", true, 21, map[string]any{"summary": "ok", "evidence_id": "e2"}),
		call("r1", "c3", "search_result", 30, "ref", "e2"),
		result("r1", "c3", true, 31, map[string]any{"summary": "命中 12"}),
		call("r1", "c4", "read_result", 40, "ref", "e2"),
		result("r1", "c4", true, 41, map[string]any{"summary": "一段"}),
		call("r1", "c5", "restart_pod", 50),
		result("r1", "c5", true, 51, map[string]any{"summary": "已重启"}),
		answer("r1", "已处理。", 60),
	}
	got := foldTasks("web-1", frames, k, nil)[0]
	want := []string{"查询监控", "关联日志", "分析数据", "执行变更", "形成结论"}
	if diff := labels(got); !eq(diff, want) {
		t.Fatalf("steps = %v, want %v", diff, want)
	}
	// The two readers are one step, because they had one purpose.
	if got.Steps[2].Calls != 2 {
		t.Errorf("the analysis step holds %d calls", got.Steps[2].Calls)
	}
}

// A tool that takes an evidence handle is working on what an earlier step
// produced, whatever it is called — the argument is the evidence.
func TestACallThatCitesEvidenceIsAnalysis(t *testing.T) {
	k := infos(map[string]ToolInfo{"summarize_evidence": known(ops.KindText)})
	got := foldTasks("web-1", []map[string]any{
		call("r1", "c1", "get_logs", 10),
		result("r1", "c1", true, 11, map[string]any{"evidence_id": "e1"}),
		call("r1", "c2", "summarize_evidence", 20, "ref", "e1"),
		result("r1", "c2", true, 21, map[string]any{"summary": "ok"}),
	}, k, nil)[0]

	if got.Steps[1].Phase != phaseAnalyze {
		t.Errorf("phase = %q, want analyze — the call cited e1", got.Steps[1].Phase)
	}
}

// An unknown tool must still show up, and must not claim to be understood.
func TestAnUnknownToolGetsAGenericStep(t *testing.T) {
	got := foldTasks("web-1", []map[string]any{
		call("r1", "c1", "some_new_thing", 10),
		result("r1", "c1", true, 11, map[string]any{"summary": "ok"}),
	}, infos(nil), nil)[0]

	if got.Steps[0].Label != "执行工具" {
		t.Errorf("label = %q", got.Steps[0].Label)
	}
	if got.Steps[0].Tools[0].Name != "some_new_thing" {
		t.Error("the tool name was lost")
	}
}

// 验收 6：a failed call fails its step and shows the real error, and the steps
// before it keep what they produced.
func TestAFailedCallFailsItsStepAndKeepsTheRest(t *testing.T) {
	k := infos(map[string]ToolInfo{"get_metrics": known(ops.KindMetricSeries), "get_logs": known(ops.KindLogExcerpt)})
	frames := []map[string]any{
		call("r1", "c1", "get_metrics", 10),
		result("r1", "c1", true, 11, map[string]any{"summary": "ok", "evidence_id": "e1"}),
		call("r1", "c2", "get_logs", 20),
		fr(frameToolResult, "round", "r1", "call_id", "c2", "ok", false, "ts", int64(21),
			"error", `dial tcp 49.234.245.200:443: i/o timeout`, "response", map[string]any{}),
	}
	got := foldTasks("web-1", frames, k, nil)[0]
	if got.Steps[0].Status != TaskCompleted || len(got.Steps[0].Artifacts) != 1 {
		t.Errorf("the earlier step lost state: %+v", got.Steps[0])
	}
	if got.Steps[1].Status != TaskFailed {
		t.Errorf("failed step = %+v", got.Steps[1])
	}
	if got.Steps[1].Error == "" || got.Steps[1].Tools[0].Error == "" {
		t.Error("the real error was dropped")
	}
	if got.Status != TaskFailed {
		t.Errorf("task = %q", got.Status)
	}
}

// 验收 9：two goals in one session are two tasks.
func TestTwoGoalsInOneSessionAreTwoTasks(t *testing.T) {
	k := infos(nil)
	frames := []map[string]any{
		fr(frameUserMessage, "text", "看下有哪些告警规则", "ts", int64(1)),
		call("r1", "c1", "list_alert_rules", 10),
		result("r1", "c1", true, 11, map[string]any{"summary": "ok"}),
		answer("r1", "共 18 条。", 20),
		fr(frameUserMessage, "text", "看下 redis 的 CPU", "ts", int64(30)),
		call("r2", "c2", "query_instant", 40),
		result("r2", "c2", true, 41, map[string]any{"summary": "ok"}),
		answer("r2", "CPU 平稳。", 50),
	}
	tasks := foldTasks("web-1", frames, k, nil)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	// 验收 6（标题）：each takes the message that opened it, not a neighbour's.
	if tasks[0].Title != "看下有哪些告警规则" || tasks[1].Title != "看下 redis 的 CPU" {
		t.Errorf("titles = %q / %q", tasks[0].Title, tasks[1].Title)
	}
	if tasks[0].Reply != "共 18 条。" || tasks[1].Reply != "CPU 平稳。" {
		t.Errorf("replies = %q / %q", tasks[0].Reply, tasks[1].Reply)
	}
}

// 验收 8：a follow-up run joined to a task belongs to it, and does not become
// a second entry telling half the story.
func TestALinkedRunJoinsItsTask(t *testing.T) {
	k := infos(nil)
	frames := []map[string]any{
		fr(frameUserMessage, "text", "巡检磁盘", "ts", int64(1)),
		call("r1", "c1", "check_disk", 10),
		result("r1", "c1", true, 11, map[string]any{"summary": "缺少阈值"}),
		answer("r1", "需要你给一个容量阈值。", 20),
		fr(frameUserMessage, "text", "阈值用 85%", "ts", int64(30)),
		call("r2", "c2", "check_disk", 40, "threshold", "85"),
		result("r2", "c2", true, 41, map[string]any{"summary": "3 台超阈值"}),
		answer("r2", "3 台机器磁盘超过 85%。", 50),
	}
	links := map[string]string{"r2": "web-1/r1"}

	tasks := foldTasks("web-1", frames, k, links)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want the follow-up folded into the original", len(tasks))
	}
	got := tasks[0]
	if got.Title != "巡检磁盘" {
		t.Errorf("title = %q; the follow-up retitled the task", got.Title)
	}
	// The reply is the latest one, because that is what the user last received.
	if got.Reply != "3 台机器磁盘超过 85%。" {
		t.Errorf("reply = %q", got.Reply)
	}
	if len(got.Runs) != 2 {
		t.Errorf("runs = %v", got.Runs)
	}
	// Two steps, not one: the task was answered and then resumed, so the
	// second check is a second stretch of work. Folding them together would
	// claim the follow-up happened before the reply it came after.
	if n := len(got.Steps); n != 4 {
		t.Errorf("steps = %v; want check → 结论 → check → 结论", labels(got))
	}
}

// Arguments are shown so a reader can see what a call was asked to do. A
// credential is not part of that, and it would otherwise be printed into a
// page anyone with the console can read.
func TestSensitiveArgumentsAreNotDisplayed(t *testing.T) {
	got := foldTasks("web-1", []map[string]any{
		call("r1", "c1", "query", 10,
			"query", "up{job=\"redis\"}", "api_token", "sk-live-abcdef", "Password", "hunter2"),
		result("r1", "c1", true, 11, map[string]any{"summary": "ok"}),
	}, infos(nil), nil)[0]

	args := got.Steps[0].Tools[0].Args
	if args["query"] != `up{job="redis"}` {
		t.Errorf("the query was hidden; it is what the call was asked to do: %v", args["query"])
	}
	for _, k := range []string{"api_token", "Password"} {
		if v, _ := args[k].(string); v != "＜已隐藏＞" {
			t.Errorf("%s = %q", k, v)
		}
	}
}

func TestLongArgumentsAreBounded(t *testing.T) {
	long := ""
	for i := 0; i < 5000; i++ {
		long += "x"
	}
	got := foldTasks("web-1", []map[string]any{
		call("r1", "c1", "query", 10, "body", long),
		result("r1", "c1", true, 11, map[string]any{"summary": "ok"}),
	}, infos(nil), nil)[0]
	if v, _ := got.Steps[0].Tools[0].Args["body"].(string); len([]rune(v)) > maxArgChars+1 {
		t.Errorf("argument is %d runes", len([]rune(v)))
	}
}

// A sub-agent's narration is part of the work, never the conclusion.
//
// Narration is the model saying what it is about to do and then doing it. It
// belongs to the step it introduced; promoting it would put a task's working
// notes where its answer goes.
func TestASubAgentsNarrationIsNotTheReply(t *testing.T) {
	got := foldTasks("web-1", []map[string]any{
		call("r1", "c1", "get_logs", 10),
		fr(frameText, "round", "r1", "branch", "root.logs", "text", "先看看日志。",
			"ts", int64(11), "final", false),
		result("r1", "c1", true, 12, map[string]any{"summary": "ok"}),
	}, infos(nil), nil)[0]

	if got.Reply != "" {
		t.Errorf("reply = %q", got.Reply)
	}
	if countLabel(got, "形成结论") != 0 {
		t.Error("a conclusion step was invented for a run that never concluded")
	}
}

// But a sub-agent's answer is the answer.
//
// A coordinator that delegates and then says nothing more is the normal shape
// of one: transfer_to_agent hands the turn over, and what the specialist says
// is what the user received. Requiring root-level text left such a task with
// no reply at all and no 形成结论 step, while the answer sat among the working
// notes — on screen the turn just ended on a token count.
func TestACoordinatorsDelegatedAnswerIsTheReply(t *testing.T) {
	got := foldTasks("web-1", []map[string]any{
		fr(frameUserMessage, "text", "查一下 CPU", "ts", int64(1)),
		fr(frameAgentTransfer, "round", "r1", "from", "OrchestrationAgent",
			"to", "MetricsQuery", "ts", int64(2)),
		call("r1", "c1", "query_instant", 10),
		result("r1", "c1", true, 11, map[string]any{"summary": "1 条曲线"}),
		fr(frameText, "round", "r1", "branch", "OrchestrationAgent.MetricsQuery",
			"text", "近 3 天 CPU 峰值 28.7%。", "ts", int64(20), "final", true),
	}, infos(nil), nil)[0]

	if got.Reply != "近 3 天 CPU 峰值 28.7%。" {
		t.Errorf("reply = %q；协调者委派后自己不再说话，专家那句就是用户收到的回答", got.Reply)
	}
	if countLabel(got, "形成结论") != 1 {
		t.Errorf("形成结论 步骤 = %d 个，应当有 1 个", countLabel(got, "形成结论"))
	}
	if got.Status != TaskCompleted {
		t.Errorf("status = %q", got.Status)
	}
}

// A run that failed with no reply shows the failure, not a report card.
func TestAFailedRunHasNoReply(t *testing.T) {
	got := foldTasks("web-1", []map[string]any{
		call("r1", "c1", "get_logs", 10),
		fr(frameError, "message", "openai chat stream: 400", "ts", int64(20)),
	}, infos(nil), nil)[0]

	if got.Reply != "" {
		t.Errorf("reply = %q", got.Reply)
	}
	if got.Status != TaskFailed || got.Error == "" {
		t.Errorf("task = %+v", got)
	}
	if countLabel(got, "形成结论") != 0 {
		t.Error("a conclusion step was invented for a failed run")
	}
}

func labels(t Task) []string {
	out := make([]string, 0, len(t.Steps))
	for _, s := range t.Steps {
		out = append(out, s.Label)
	}
	return out
}

func countLabel(t Task, label string) int {
	n := 0
	for _, s := range t.Steps {
		if s.Label == label {
			n++
		}
	}
	return n
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The store's own readers are analysis by identity, not only by their
// arguments.
//
// In practice they are always called with a ref, so the argument rule covers
// them — and that is exactly why this needs its own case: with the arguments
// missing (an older event, a frame that lost them) the identity is all that is
// left, and without it a read-back would be filed as fresh collection.
func TestReadersAreAnalysisEvenWithoutVisibleArguments(t *testing.T) {
	got := foldTasks("web-1", []map[string]any{
		call("r1", "c1", "get_logs", 10),
		result("r1", "c1", true, 11, map[string]any{"evidence_id": "e1"}),
		// No args at all, as an event written before they were projected.
		fr(frameToolCall, "round", "r1", "call_id", "c2", "name", "read_result", "ts", int64(20)),
		result("r1", "c2", true, 21, map[string]any{"summary": "一段"}),
		fr(frameToolCall, "round", "r1", "call_id", "c3", "name", "search_result", "ts", int64(30)),
		result("r1", "c3", true, 31, map[string]any{"summary": "命中 3"}),
	}, infos(nil), nil)[0]

	if len(got.Steps) != 2 {
		t.Fatalf("steps = %v, want fetch then analysis", labels(got))
	}
	if got.Steps[1].Phase != phaseAnalyze || got.Steps[1].Label != "分析数据" {
		t.Errorf("the read-back was filed as %q/%q", got.Steps[1].Phase, got.Steps[1].Label)
	}
	if got.Steps[1].Calls != 2 {
		t.Errorf("the two readers did not share a step: %+v", got.Steps[1])
	}
}

// An interrupted run resumed by a follow-up puts both runs' calls in one step,
// and both runs number their first call c1.
//
// This is the case where matching a result to its call by id alone goes wrong
// inside a single step: the follow-up's answer lands on the interrupted run's
// call, which then reads as complete while the call that actually produced it
// stays "执行中". The step boundary hides this whenever the first run got as
// far as a reply, which is why it needs the interrupted shape to show.
func TestAResumedRunsResultLandsOnItsOwnCall(t *testing.T) {
	k := infos(nil)
	frames := []map[string]any{
		fr(frameUserMessage, "text", "查一下磁盘", "ts", int64(1)),
		call("r1", "c1", "check_disk", 10),
		// r1 never answered — the run was cut off, which is the usual reason
		// someone resumes a task rather than asking a fresh question.
		fr(frameUserMessage, "text", "继续", "ts", int64(30)),
		call("r2", "c1", "check_disk", 40),
		result("r2", "c1", true, 41, map[string]any{"summary": "3 台超阈值"}),
		answer("r2", "3 台机器磁盘满了。", 50),
	}
	tasks := foldTasks("web-1", frames, k, map[string]string{"r2": "web-1/r1"})
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	var work *Step
	for i := range tasks[0].Steps {
		if len(tasks[0].Steps[i].Tools) > 0 {
			work = &tasks[0].Steps[i]
			break
		}
	}
	if work == nil || len(work.Tools) != 2 {
		t.Fatalf("steps = %+v; want one step holding both runs' calls", tasks[0].Steps)
	}
	first, second := work.Tools[0], work.Tools[1]
	if first.Round != "r1" || !first.Pending {
		t.Errorf("被打断那一轮的调用应当仍是执行中: %+v", first)
	}
	if second.Round != "r2" || second.Pending || second.Summary != "3 台超阈值" {
		t.Errorf("续跑那一轮的结果没有落到自己的调用上: %+v", second)
	}
}

// A question refused before any tool ran is still a task, and a failed one.
//
// It produced no tool call, no answer, no stored result — so nothing else in
// the frame stream belongs to that run, and the failure had nothing to attach
// itself to. It was dropped, and the run disappeared from the board: the one
// state a person actually has to do something about was the one the task
// centre would not show.
func TestARefusedQuestionIsAFailedTask(t *testing.T) {
	frames := []map[string]any{
		fr(frameUserMessage, "text", "把生产库删了", "ts", int64(1)),
		fr(frameError, "round", "r1", "ts", int64(5),
			"message", "模型因安全策略拒绝了这次回答", "code", "SAFETY"),
	}
	tasks := foldTasks("web-1", frames, infos(nil), nil)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d; 被拒绝的提问从任务中心消失了", len(tasks))
	}
	got := tasks[0]
	if got.Status != TaskFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Error == "" {
		t.Error("失败任务没有带上原因，看板上只剩一个红点")
	}
	if got.Title != "把生产库删了" {
		t.Errorf("title = %q; 开启这一轮的提问就是它的标题", got.Title)
	}
	// And it is admitted: no tool ran, so the chat filter would drop it if the
	// status did not say otherwise.
	if !IsTask(got, noRecords) {
		t.Error("失败的任务被当成普通对话过滤掉了")
	}
}

// A run that was refused and then answered is not a failed run.
//
// The live stream already knew this — it only fails a turn that never answered
// — but the projection applied the failure the moment it saw it. The same run
// therefore read as completed while it streamed and as failed on every reload
// afterwards, which is worse than either answer alone.
func TestARecoveredRunDoesNotStayFailedOnReload(t *testing.T) {
	frames := []map[string]any{
		fr(frameUserMessage, "text", "看下告警", "ts", int64(1)),
		call("r1", "c1", "list_alert_rules", 5),
		result("r1", "c1", true, 6, map[string]any{"summary": "3 条"}),
		fr(frameError, "round", "r1", "ts", int64(8), "message", "模型未生成内容", "code", "OTHER"),
		answer("r1", "共 3 条告警规则。", 12),
	}
	tasks := foldTasks("web-1", frames, infos(nil), nil)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	if tasks[0].Status != TaskCompleted {
		t.Errorf("status = %q; 这一轮最后是答复了的", tasks[0].Status)
	}
	if tasks[0].Reply != "共 3 条告警规则。" {
		t.Errorf("reply = %q", tasks[0].Reply)
	}
}

// And a failure in one run of a folded task does not spare that task when the
// other run answered — the failed run still never answered.
func TestOnlyTheUnansweredRunOfAFoldedTaskFails(t *testing.T) {
	frames := []map[string]any{
		fr(frameUserMessage, "text", "巡检磁盘", "ts", int64(1)),
		call("r1", "c1", "check_disk", 5),
		result("r1", "c1", true, 6, map[string]any{"summary": "缺少阈值"}),
		answer("r1", "需要一个阈值。", 10),
		fr(frameUserMessage, "text", "用 85%", "ts", int64(20)),
		call("r2", "c1", "check_disk", 24),
		fr(frameError, "round", "r2", "ts", int64(28), "message", "模型未生成内容"),
	}
	tasks := foldTasks("web-1", frames, infos(nil), map[string]string{"r2": "web-1/r1"})
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	if tasks[0].Status != TaskFailed {
		t.Errorf("status = %q; 续跑那一轮没有答复，任务没有完成", tasks[0].Status)
	}
}

// The clearing is per run, not per task.
//
// A task whose first run was refused and whose follow-up answered still
// contains a run that never answered, and that is what the failure is about.
// Clearing it because some later run replied would report the task as clean
// while the thing the user came back to fix is still in it.
func TestAnAnswerClearsOnlyItsOwnRunsFailure(t *testing.T) {
	frames := []map[string]any{
		fr(frameUserMessage, "text", "巡检磁盘", "ts", int64(1)),
		call("r1", "c1", "check_disk", 5),
		fr(frameError, "round", "r1", "ts", int64(8), "message", "模型未生成内容"),
		fr(frameUserMessage, "text", "再试一次", "ts", int64(20)),
		call("r2", "c1", "check_disk", 24),
		result("r2", "c1", true, 25, map[string]any{"summary": "3 台超阈值"}),
		answer("r2", "3 台机器磁盘满了。", 30),
	}
	tasks := foldTasks("web-1", frames, infos(nil), map[string]string{"r2": "web-1/r1"})
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	if tasks[0].Status != TaskFailed {
		t.Errorf("status = %q; 第一轮没有答复，后一轮的答复不代表它成功了", tasks[0].Status)
	}
}

// The two answers the records probe can give, as the callable IsTask now
// takes. A function because answering it for real costs a query, and IsTask
// asks only when nothing cheaper has settled the question.
func noRecords() bool   { return false }
func someRecords() bool { return true }

// 协调者跑一步、专家跑同一类的一步，不能并成一条：步骤是挂在某个 agent 名下
// 展示的，并了就会把一个人的活记在另一个人头上，而换手——委派运行里最值得看
// 的那一刻——一点痕迹都不留。
func TestAStepDoesNotSpanTwoAgents(t *testing.T) {
	callBy := func(agent, callID string, ts int64) map[string]any {
		return fr(frameToolCall, "round", "r1", "call_id", callID,
			"name", "query_instant", "ts", ts, "agent", agent,
			"args", map[string]any{})
	}
	got := foldTasks("web-1", []map[string]any{
		callBy("OrchestrationAgent", "c1", 10),
		result("r1", "c1", true, 11, map[string]any{"summary": "ok"}),
		callBy("MetricsQuery", "c2", 20),
		result("r1", "c2", true, 21, map[string]any{"summary": "ok"}),
	}, infos(nil), nil)[0]

	if len(got.Steps) != 2 {
		t.Fatalf("步骤 = %d 条，两个 agent 的活被并成一条了: %v", len(got.Steps), labels(got))
	}
	if got.Steps[0].Agent != "OrchestrationAgent" || got.Steps[1].Agent != "MetricsQuery" {
		t.Errorf("归属错了: %q / %q", got.Steps[0].Agent, got.Steps[1].Agent)
	}
}
