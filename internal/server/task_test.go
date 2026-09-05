package server

import (
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// kinds is a stand-in for the tool registry: declared tools report what they
// produce, everything else reports nothing — which is what every MCP tool does
// today, since adopted metadata carries no Produces.
func kinds(m map[string]ops.EvidenceKind) func(string) ops.EvidenceKind {
	return func(tool string) ops.EvidenceKind { return m[tool] }
}

func fr(typ string, kv ...any) map[string]any {
	f := map[string]any{"type": typ}
	for i := 0; i+1 < len(kv); i += 2 {
		f[kv[i].(string)] = kv[i+1]
	}
	return f
}

func call(round, callID, name string, ts int64) map[string]any {
	return fr(frameToolCall, "round", round, "call_id", callID, "name", name, "ts", ts, "agent", "root")
}

func result(round, callID string, ok bool, ts int64, resp map[string]any) map[string]any {
	return fr(frameToolResult, "round", round, "call_id", callID, "ok", ok, "ts", ts, "response", resp)
}

// A step is a stretch of one kind of work, so consecutive calls of the same
// kind are one step and a change of kind starts another.
func TestConsecutiveSameKindCallsAreOneStep(t *testing.T) {
	k := kinds(map[string]ops.EvidenceKind{
		"get_metrics": ops.KindMetricSeries,
		"get_alerts":  ops.KindMetricSeries,
		"get_logs":    ops.KindLogExcerpt,
	})
	frames := []map[string]any{
		fr(frameUserMessage, "text", "排查支付服务 CPU 异常", "ts", int64(1)),
		call("r1", "c1", "get_metrics", 10),
		result("r1", "c1", true, 20, map[string]any{"summary": "3 个实例超阈值", "evidence_id": "e1"}),
		call("r1", "c2", "get_alerts", 30),
		result("r1", "c2", true, 40, map[string]any{"summary": "2 条告警", "evidence_id": "e2"}),
		call("r1", "c3", "get_logs", 50),
		result("r1", "c3", true, 60, map[string]any{"summary": "命中 12 行", "evidence_id": "e3"}),
		fr(frameText, "round", "r1", "text", "根因是连接池耗尽。", "ts", int64(70)),
	}

	tasks := foldTasks("web-1", frames, k)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	got := tasks[0]
	if got.Title != "排查支付服务 CPU 异常" {
		t.Errorf("title = %q", got.Title)
	}
	if len(got.Steps) != 3 {
		t.Fatalf("steps = %d, want 查询指标 / 关联日志 / 形成结论: %+v", len(got.Steps), labels(got))
	}
	if got.Steps[0].Label != "查询指标" || got.Steps[0].Calls != 2 {
		t.Errorf("first step = %+v", got.Steps[0])
	}
	if got.Steps[1].Label != "关联日志" || got.Steps[1].Calls != 1 {
		t.Errorf("second step = %+v", got.Steps[1])
	}
	if got.Steps[2].Label != "形成结论" {
		t.Errorf("last step = %+v", got.Steps[2])
	}
	// The artifacts of a step are the handles its tools produced, so clicking
	// a step can point at what it made.
	if len(got.Steps[0].Artifacts) != 2 || got.Steps[0].Artifacts[0] != "e1" {
		t.Errorf("artifacts = %v", got.Steps[0].Artifacts)
	}
	if got.Type != TypeMonitor {
		t.Errorf("type = %q, want monitor", got.Type)
	}
	if got.Status != TaskCompleted {
		t.Errorf("status = %q", got.Status)
	}
}

// Every MCP tool today declares nothing, so grouping has to degrade to
// something deterministic rather than putting the whole run in one bucket.
func TestToolsWithNoDeclaredKindGroupByTool(t *testing.T) {
	k := kinds(nil) // nothing declares anything
	frames := []map[string]any{
		fr(frameUserMessage, "text", "查告警", "ts", int64(1)),
		call("r1", "c1", "list_alert_rules", 10),
		result("r1", "c1", true, 20, map[string]any{"summary": "ok"}),
		call("r1", "c2", "list_alert_rules", 30),
		result("r1", "c2", true, 40, map[string]any{"summary": "ok"}),
		call("r1", "c3", "get_service_logs", 50),
		result("r1", "c3", true, 60, map[string]any{"summary": "ok"}),
	}
	tasks := foldTasks("web-1", frames, k)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	steps := tasks[0].Steps
	if len(steps) != 2 {
		t.Fatalf("steps = %v, want two grouped by tool", labels(tasks[0]))
	}
	if steps[0].Label != "list_alert_rules" || steps[0].Calls != 2 {
		t.Errorf("first = %+v", steps[0])
	}
	if steps[1].Label != "get_service_logs" || steps[1].Calls != 1 {
		t.Errorf("second = %+v", steps[1])
	}
	if steps[0].Kind != "" {
		t.Errorf("kind = %q, want empty rather than invented", steps[0].Kind)
	}
}

// A failed tool fails its step, and the task — but the steps before it keep
// their state, so what was already produced stays visible.
func TestAFailedToolFailsItsStepAndNotTheOnesBefore(t *testing.T) {
	k := kinds(map[string]ops.EvidenceKind{"get_metrics": ops.KindMetricSeries, "get_logs": ops.KindLogExcerpt})
	frames := []map[string]any{
		fr(frameUserMessage, "text", "排查", "ts", int64(1)),
		call("r1", "c1", "get_metrics", 10),
		result("r1", "c1", true, 20, map[string]any{"summary": "ok", "evidence_id": "e1"}),
		call("r1", "c2", "get_logs", 30),
		result("r1", "c2", false, 40, nil),
	}
	frames[len(frames)-1]["error"] = "dial tcp: i/o timeout"

	got := foldTasks("web-1", frames, k)[0]
	if len(got.Steps) != 2 {
		t.Fatalf("steps = %v", labels(got))
	}
	if got.Steps[0].Status != TaskCompleted {
		t.Errorf("the step before the failure = %q", got.Steps[0].Status)
	}
	if len(got.Steps[0].Artifacts) != 1 {
		t.Error("a completed step lost its artifact when a later step failed")
	}
	if got.Steps[1].Status != TaskFailed || got.Steps[1].Error == "" {
		t.Errorf("failed step = %+v", got.Steps[1])
	}
	if got.Status != TaskFailed {
		t.Errorf("task status = %q", got.Status)
	}
}

// One invocation is one task, so a session with three questions is three tasks.
func TestEachInvocationIsItsOwnTask(t *testing.T) {
	k := kinds(nil)
	frames := []map[string]any{
		fr(frameUserMessage, "text", "第一个问题", "ts", int64(1)),
		call("r1", "c1", "get_pods", 10),
		result("r1", "c1", true, 20, map[string]any{"summary": "ok"}),
		fr(frameText, "round", "r1", "text", "答案一", "ts", int64(30)),
		fr(frameUserMessage, "text", "第二个问题", "ts", int64(40)),
		call("r2", "c2", "get_logs", 50),
		result("r2", "c2", true, 60, map[string]any{"summary": "ok"}),
		fr(frameText, "round", "r2", "text", "答案二", "ts", int64(70)),
	}
	tasks := foldTasks("web-1", frames, k)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(tasks))
	}
	if tasks[0].Title != "第一个问题" || tasks[1].Title != "第二个问题" {
		t.Errorf("titles = %q, %q", tasks[0].Title, tasks[1].Title)
	}
	if tasks[0].ID != "web-1/r1" || tasks[1].ID != "web-1/r2" {
		t.Errorf("ids = %q, %q", tasks[0].ID, tasks[1].ID)
	}
	if tasks[0].Answer != "答案一" {
		t.Errorf("answer = %q", tasks[0].Answer)
	}
}

// A transfer does not do work, so it must not become a step — otherwise "this
// task took five steps" counts handoffs as work.
func TestAgentTransferProducesNoStep(t *testing.T) {
	k := kinds(nil)
	frames := []map[string]any{
		fr(frameUserMessage, "text", "问题", "ts", int64(1)),
		fr(frameAgentTransfer, "round", "r1", "from", "root", "to", "logs", "ts", int64(5)),
		call("r1", "c1", "get_logs", 10),
		result("r1", "c1", true, 20, map[string]any{"summary": "ok"}),
	}
	got := foldTasks("web-1", frames, k)[0]
	if len(got.Steps) != 1 {
		t.Errorf("steps = %v, want only the tool step", labels(got))
	}
}

// A sub-agent's prose is part of the work, not the conclusion.
func TestOnlyTheTopLevelAnswerClosesTheTask(t *testing.T) {
	k := kinds(nil)
	frames := []map[string]any{
		fr(frameUserMessage, "text", "问题", "ts", int64(1)),
		call("r1", "c1", "get_logs", 10),
		result("r1", "c1", true, 20, map[string]any{"summary": "ok"}),
		fr(frameText, "round", "r1", "branch", "root.logs", "text", "子 agent 的中间话", "ts", int64(30)),
		fr(frameText, "round", "r1", "text", "最终结论", "ts", int64(40)),
	}
	got := foldTasks("web-1", frames, k)[0]
	if got.Answer != "最终结论" {
		t.Errorf("answer = %q; a sub-agent's prose was taken as the conclusion", got.Answer)
	}
	if n := countLabel(got, "形成结论"); n != 1 {
		t.Errorf("结论 steps = %d, want exactly 1", n)
	}

	// The discriminating case: a run whose only prose came from a sub-agent
	// has no conclusion. Ordering alone does not test the guard, because a
	// later top-level answer overwrites an earlier mistake anyway.
	subOnly := foldTasks("web-1", []map[string]any{
		fr(frameUserMessage, "text", "问题", "ts", int64(1)),
		call("r1", "c1", "get_logs", 10),
		result("r1", "c1", true, 20, map[string]any{"summary": "ok"}),
		fr(frameText, "round", "r1", "branch", "root.logs", "text", "子 agent 说了点什么", "ts", int64(30)),
	}, k)[0]
	if subOnly.Answer != "" {
		t.Errorf("answer = %q; a sub-agent's prose became the task's conclusion", subOnly.Answer)
	}
	if n := countLabel(subOnly, "形成结论"); n != 0 {
		t.Errorf("结论 steps = %d on a run that never concluded", n)
	}
}

// A scheduled run is an inspection because of where it ran, which is a fact,
// not because of what its text looks like.
func TestAScheduledRunIsAnInspection(t *testing.T) {
	k := kinds(map[string]ops.EvidenceKind{"get_logs": ops.KindLogExcerpt})
	frames := []map[string]any{
		call("r1", "c1", "get_logs", 10),
		result("r1", "c1", true, 20, map[string]any{"summary": "ok"}),
	}
	got := foldTasks("schedule-nightly", frames, k)[0]
	if got.Type != TypeInspection {
		t.Errorf("type = %q, want inspection", got.Type)
	}
	// It has no user message, so the title falls back to the work itself
	// rather than borrowing another task's.
	if got.Title != "关联日志" {
		t.Errorf("title = %q", got.Title)
	}
}

// The withheld/truncated/retrievable flags are the gateway's own words to the
// model. Re-deriving them in the browser would be a second opinion about what
// the model was told.
func TestStepToolsCarryWhatTheModelWasTold(t *testing.T) {
	k := kinds(nil)
	frames := []map[string]any{
		call("r1", "c1", "get_service_logs", 10),
		result("r1", "c1", true, 20, map[string]any{
			"summary": "日志", "evidence_id": "e9", "retrievable": true, "truncated": true,
			"overview": map[string]any{"bytes": 4816496, "lines": 60000, "note": "完整内容未进入本次上下文（本轮剩余预算不足），已保存，可搜索与分段读取。"},
		}),
	}
	got := foldTasks("web-1", frames, k)[0]
	tool := got.Steps[0].Tools[0]
	if !tool.Retrievable || !tool.Truncated || !tool.Withheld {
		t.Errorf("tool = %+v", tool)
	}
	if tool.Bytes != 4816496 || tool.Lines != 60000 {
		t.Errorf("scale = %d bytes / %d lines", tool.Bytes, tool.Lines)
	}
	if tool.EvidenceID != "e9" {
		t.Errorf("evidence = %q", tool.EvidenceID)
	}
}

// An unanswered tool leaves its step running — that is what an in-flight task
// looks like from stored events.
func TestAnUnansweredToolLeavesTheStepRunning(t *testing.T) {
	k := kinds(nil)
	frames := []map[string]any{
		call("r1", "c1", "get_logs", 10),
	}
	got := foldTasks("web-1", frames, k)[0]
	if got.Steps[0].Status != TaskRunning {
		t.Errorf("status = %q, want running", got.Steps[0].Status)
	}
	if !got.Steps[0].Tools[0].Pending {
		t.Error("the tool is not marked pending")
	}
}

func TestAnErrorFrameFailsTheTask(t *testing.T) {
	k := kinds(nil)
	frames := []map[string]any{
		call("r1", "c1", "get_logs", 10),
		result("r1", "c1", true, 20, map[string]any{"summary": "ok"}),
		fr(frameError, "message", "openai chat stream: 400", "ts", int64(30)),
	}
	got := foldTasks("web-1", frames, k)[0]
	if got.Status != TaskFailed || got.Error == "" {
		t.Errorf("task = %+v", got)
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
