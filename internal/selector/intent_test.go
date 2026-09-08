package selector

import (
	"strings"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

func produces(k ops.EvidenceKind) func(*ops.ToolMetadata) {
	return func(m *ops.ToolMetadata) { m.Produces = k }
}

// A third-party server's tools must be reachable from a question written in
// another language.
//
// The regression this guards is not subtle once seen. An MCP server ships
// English names and English prose and nothing else; the questions are Chinese;
// the token sets never intersect, so every one of that server's tools scores
// zero and ranking falls through to declaration order. Registration sorts by
// key, so whichever tools sort last are cut first — for every question, not
// just this one. In the real deployment that was the three PromQL query tools,
// and the model spent nine calls concluding the capability did not exist.
func TestAChineseQuestionReachesAnEnglishOnlyTool(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("get_notify_channel", desc("Get a notification channel by id.")),
		meta("list_roles", desc("List roles.")),
		meta("list_users", desc("List users.")),
		meta("query_range", desc("Run a PromQL range query against a Prometheus-compatible datasource.")),
	}
	res := Select("查询 algo-data redis 最近 3 天的 cpu 使用率", tools, Config{})

	if got := topRanked(res); got != "query_range" {
		t.Errorf("排在最前的是 %q，应当是 query_range", got)
	}
	var scored int
	for _, c := range res.Candidates {
		if c.Matched {
			scored++
		}
	}
	if scored == 0 {
		t.Fatal("中文提问对纯英文工具一个分都打不出来——排序会退化成字母序")
	}
}

// Under a binding budget, that has to mean the tool survives the cut.
func TestTheRelevantToolSurvivesTheBudget(t *testing.T) {
	// Zero-scoring filler that sorts before "query_*", which is the shape that
	// made the query tools the deterministic casualties.
	var tools []ops.ToolMetadata
	for _, n := range []string{
		"get_notify_channel", "get_notify_rule", "list_alert_subscribes", "list_mutes",
		"list_notify_channels", "list_notify_rules", "list_operations", "list_roles",
	} {
		tools = append(tools, meta(n, desc("Administrative endpoint.")))
	}
	tools = append(tools,
		meta("query_range", desc("Run a PromQL range query against a Prometheus-compatible datasource.")),
		meta("query_instant", desc("Run a PromQL instant query against a Prometheus-compatible datasource."),
			produces(ops.KindMetricSeries)),
	)

	res := Select("查一下这个实例最近的 cpu 使用率曲线", tools, Config{MaxTools: 5})
	for _, want := range []string{"query_range", "query_instant"} {
		if rankOf(res, want) < 0 {
			t.Errorf("%s 被预算砍掉了，而它正是能回答这个问题的工具: 入选=%v", want, res.Selected)
		}
	}
	// 曲线 implies "range", so query_range outscoring query_instant here is
	// the mechanism working, not a defect — a curve does want a range query.
	if rankOf(res, "query_range") < 0 {
		t.Error("要曲线的问题反而没选上 query_range")
	}
}

// A declared produces is worth something on its own, held apart from every
// other signal: two tools identical but for that field must not tie.
func TestADeclaredProducesEarnsScoreByItself(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("alpha_probe", desc("Opaque third-party endpoint.")),
		meta("beta_probe", desc("Opaque third-party endpoint."), produces(ops.KindMetricSeries)),
	}
	res := Select("看下 cpu 使用率", tools, Config{})
	if got := topRanked(res); got != "beta_probe" {
		t.Errorf("排在最前的是 %q；声明了 produces=metric_series 的是 beta_probe", got)
	}
	var alpha, beta float64
	for _, c := range res.Candidates {
		switch c.Tool {
		case "alpha_probe":
			alpha = c.Score
		case "beta_probe":
			beta = c.Score
		}
	}
	if beta-alpha != wKind {
		t.Errorf("produces 带来的分差是 %.2f，应当正好是 wKind=%.2f", beta-alpha, wKind)
	}
}

// Inference must never outrank testimony. Somebody writing down that a tool
// answers this kind of question, in the words the question used, is stronger
// evidence than the question merely smelling like a metrics question.
func TestAnInferredMatchNeverOutranksALiteralOne(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("query_range", desc("Run a PromQL range query."), produces(ops.KindMetricSeries)),
		meta("grab_curve", use("查询 cpu 使用率曲线")),
	}
	res := Select("查询 cpu 使用率", tools, Config{})
	if got := topRanked(res); got != "grab_curve" {
		t.Errorf("排在最前的是 %q；写明了适用场景的那个才该在前", got)
	}
}

// And it must not get there by accumulation either.
//
// The first version multiplied each inferred weight by the number of matching
// tokens, so a tool with several plausible English words in its name added up
// past a literal match: query_range_promql_metric_usage scored 16.0 against a
// literal name match's 12.0. One test with one stem per tool passed the whole
// time, which is why this one stuffs the name on purpose.
func TestInferenceCannotAccumulatePastALiteralMatch(t *testing.T) {
	stuffed := "query_range_instant_promql_metric_metrics_usage_util_latency_memory"
	tools := []ops.ToolMetadata{
		meta("使用率", desc("命中问题里的字面词")),
		meta(stuffed, desc("promql query range instant metric metrics usage util latency memory"),
			produces(ops.KindMetricSeries)),
	}
	res := Select("看下 cpu 使用率", tools, Config{})
	if got := topRanked(res); got != "使用率" {
		t.Errorf("排在最前的是 %q；字面命中名称的那个才该在前", got)
	}

	var literal, inferred float64
	for _, c := range res.Candidates {
		switch c.Tool {
		case "使用率":
			literal = c.Score
		case stuffed:
			inferred = c.Score
		}
	}
	if inferred > maxInferred {
		t.Errorf("推断得分 %.2f 超出了上界 %.2f——说明它还在按 token 数累加", inferred, maxInferred)
	}
	if inferred >= literal {
		t.Errorf("推断 %.2f 追上了字面 %.2f", inferred, literal)
	}
}

// The bound is a checked property, not an arithmetic coincidence that survives
// until somebody bumps a weight.
func TestInferenceIsBoundedBelowADeliberateDeclaration(t *testing.T) {
	if maxInferred >= wUseCase {
		t.Errorf("maxInferred=%.2f 不低于 wUseCase=%.2f：写下 use_cases 的工具会被纯推断压过",
			maxInferred, wUseCase)
	}
	if maxInferred >= wName {
		t.Errorf("maxInferred=%.2f 不低于 wName=%.2f", maxInferred, wName)
	}
	// It does have to beat an incidental mention in prose, or it changes
	// nothing in the case it exists for.
	if maxInferred <= wDescription {
		t.Errorf("maxInferred=%.2f 不高于 wDescription=%.2f：推断压不过一句顺带提及，等于没做",
			maxInferred, wDescription)
	}
}

// 使用 is not a metrics word. It sits in 怎么使用、使用说明、该使用哪个数据源,
// and while it was a cue every one of those inferred metric_series — which
// promotes the PromQL tools onto questions that have nothing to do with them.
func TestOverBroadWordsAreNotCues(t *testing.T) {
	for _, q := range []string{
		"这个工具怎么使用",
		"使用说明在哪",
		"该使用哪个数据源",
		"帮我看看有哪些通知渠道",
	} {
		if in := inferIntent(tokenize(q)); !in.Empty() {
			t.Errorf("%q 推断出了 kinds=%v stems=%v，它不是一个指标问题", q, in.kinds, in.stems)
		}
	}
	// The discriminating half still works, in both words it comes from.
	for _, q := range []string{"cpu 使用率", "内存利用率"} {
		if in := inferIntent(tokenize(q)); !in.kinds[ops.KindMetricSeries] {
			t.Errorf("%q 没有被识别为指标问题", q)
		}
	}
}

// A Chinese cue key longer than two characters can never be produced by
// tokenize, so it is a dead entry that reads like a working one.
func TestEveryChineseCueKeyIsATokenTokenizeCanProduce(t *testing.T) {
	for key := range cues {
		runes := []rune(key)
		cjk := false
		for _, r := range runes {
			if r > 127 {
				cjk = true
				break
			}
		}
		if !cjk {
			continue // Latin keys are whole words
		}
		if len(runes) != 2 {
			t.Errorf("cue %q 有 %d 个字：tokenize 只产二字组，这一行永远命中不了", key, len(runes))
			continue
		}
		// Belt and braces: the key must actually come back out of tokenize.
		if !tokenize(key)[key] {
			t.Errorf("cue %q 没有出现在 tokenize(%q) 的结果里", key, key)
		}
	}
}

// A question the lexicon says nothing about behaves exactly as before, so a
// deployment that never hits these words sees no change at all.
func TestAQuestionWithNoCuesIsUnchanged(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("alpha", desc("Something.")),
		meta("beta", desc("Something else.")),
	}
	q := "帮我看看有哪些通知渠道"
	if in := inferIntent(tokenize(q)); !in.Empty() {
		t.Fatalf("这个问题不该推断出任何意图: kinds=%v stems=%v", in.kinds, in.stems)
	}
	res := Select(q, tools, Config{})
	for _, c := range res.Candidates {
		if c.Matched {
			t.Errorf("%s 打出了分，说明词表比预期宽", c.Tool)
		}
	}
}

// A stem the question already used is not added, or the same evidence would be
// counted twice — once as a literal name match and once as an inferred one.
func TestAStemTheQuestionAlreadyUsedIsNotAdded(t *testing.T) {
	in := inferIntent(tokenize("promql query 怎么写"))
	if in.stems["query"] || in.stems["promql"] {
		t.Errorf("问题里已经出现的词又被当成推断信号: %v", in.stems)
	}
	// The kind is still inferred; only the duplicate stems are dropped.
	if !in.kinds[ops.KindMetricSeries] {
		t.Error("kind 没有推断出来")
	}
}

// The reason string has to name which signal fired, because the failure this
// whole mechanism introduces is "the wrong tool was promoted" and the first
// question is always which row of the lexicon did it.
func TestTheReasonNamesTheInferredSignal(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("query_range", desc("Run a PromQL range query."), produces(ops.KindMetricSeries)),
	}
	res := Select("看下 cpu 使用率", tools, Config{})
	r := res.Candidates[0].Reason
	for _, want := range []string{"产出类型", "推断"} {
		if !strings.Contains(r, want) {
			t.Errorf("reason = %q，没有说明 %q 这个信号", r, want)
		}
	}
}

// A conversation is continuous; a question is not.
//
// By the third turn nobody repeats 指标 or CPU. "集群 id 我不清楚，你给我下"
// is still a metrics question and infers nothing at all on its own — which put
// the PromQL tools back at the bottom of a flat ranking, where the budget cut
// them, and the model reported (correctly) that it had no way to query. The
// capability had been taken away by the wording of a follow-up.
func TestACarriedIntentKeepsTheToolsAFollowUpStoppedNaming(t *testing.T) {
	var tools []ops.ToolMetadata
	for _, n := range []string{
		"get_notify_channel", "list_alert_subscribes", "list_mutes",
		"list_notify_rules", "list_roles", "list_users",
	} {
		tools = append(tools, meta(n, desc("Administrative endpoint.")))
	}
	tools = append(tools,
		meta("query_range", desc("Run a PromQL range query against a Prometheus-compatible datasource.")),
		meta("query_instant", desc("Run a PromQL instant query against a Prometheus-compatible datasource."),
			produces(ops.KindMetricSeries)),
	)

	const followUp = "集群 id 我不清楚，你给我下"
	// On its own it says nothing, and under a binding budget the query tools
	// are the ones cut — they sort last among a field of zeroes.
	alone := Select(followUp, tools, Config{MaxTools: 5})
	if rankOf(alone, "query_range") >= 0 || rankOf(alone, "query_instant") >= 0 {
		t.Fatalf("前提不成立：这句话单独看本就该丢掉查询工具，实际入选=%v", alone.Selected)
	}

	// Carried from an earlier turn of the same conversation, they stay.
	carried := Infer("查一下这个实例最近 3 天的 cpu 使用率")
	if carried.Empty() {
		t.Fatal("前一轮那句话没有推断出任何意图")
	}
	withHistory := Select(followUp, tools, Config{MaxTools: 5, Carried: carried})
	for _, want := range []string{"query_range", "query_instant"} {
		if rankOf(withHistory, want) < 0 {
			t.Errorf("%s 在追问里丢了；这次对话一直在问指标: 入选=%v", want, withHistory.Selected)
		}
	}
}

// Merge is a union and mutates neither side: the caller holds one of these per
// session, and a selection must not be able to grow somebody else's record.
func TestMergingIntentsDoesNotMutateEither(t *testing.T) {
	a := Infer("cpu 使用率")
	b := Infer("看下日志")
	merged := a.Merge(b)

	if !merged.kinds[ops.KindMetricSeries] || !merged.kinds[ops.KindLogExcerpt] {
		t.Errorf("并集不完整: %v", merged.kinds)
	}
	if a.kinds[ops.KindLogExcerpt] {
		t.Error("Merge 改写了左边")
	}
	if b.kinds[ops.KindMetricSeries] {
		t.Error("Merge 改写了右边")
	}
	// And an empty intent merges cleanly in both directions.
	if got := (Intent{}).Merge(a); !got.kinds[ops.KindMetricSeries] {
		t.Error("空意图与非空合并丢了内容")
	}
	if got := a.Merge(Intent{}); !got.kinds[ops.KindMetricSeries] {
		t.Error("非空与空意图合并丢了内容")
	}
}
