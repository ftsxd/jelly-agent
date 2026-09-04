package selector

import (
	"slices"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

func meta(name string, mods ...func(*ops.ToolMetadata)) ops.ToolMetadata {
	m := ops.ToolMetadata{Name: name}
	for _, f := range mods {
		f(&m)
	}
	return m
}

func use(cases ...string) func(*ops.ToolMetadata) {
	return func(m *ops.ToolMetadata) { m.UseCases = cases }
}
func desc(d string) func(*ops.ToolMetadata) {
	return func(m *ops.ToolMetadata) { m.Description = d }
}
func anti(cases ...string) func(*ops.ToolMetadata) {
	return func(m *ops.ToolMetadata) { m.AntiExamples = cases }
}
func fallback(m *ops.ToolMetadata)  { m.Fallback = true }
func baseline(m *ops.ToolMetadata)  { m.Baseline = true }
func tags(t ...string) func(*ops.ToolMetadata) {
	return func(m *ops.ToolMetadata) { m.Tags = t }
}

func rankOf(r Result, tool string) int {
	return slices.Index(r.Selected, tool)
}

// topRanked is the tool ranking put first. Selected is deliberately in the
// catalogue's declared order, so a test about ranking has to read Candidates.
func topRanked(r Result) string {
	if len(r.Candidates) == 0 {
		return ""
	}
	return r.Candidates[0].Tool
}

// The signal has to survive a Chinese sentence, because that is what the
// questions are written in. A whitespace tokenizer sees "看下这个服务的日志"
// as one token and matches nothing at all.
func TestChineseQuestionRanksTheMatchingTool(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("fetch_url", use("读取网页正文")),
		meta("get_logs", use("查看服务日志"), desc("拉取容器日志")),
		meta("remember", use("记录用户偏好")),
	}
	r := Select("帮我看下这个服务的日志", tools, Config{})
	if got := topRanked(r); got != "get_logs" {
		t.Errorf("top = %q, want get_logs; 候选 %v", got, r.Candidates)
	}
}

// An ops question names its subject in ASCII even inside a Chinese sentence.
func TestLatinTermInsideChineseMatches(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("get_pods", use("列出 pod")),
		meta("query_mysql", use("查询 MySQL")),
	}
	r := Select("看下 mysql 的连接数", tools, Config{})
	if got := topRanked(r); got != "query_mysql" {
		t.Errorf("top = %q, want query_mysql", got)
	}
}

// An anti-example was written specifically to say "not this", and the tools
// that need excluding are the ones that otherwise look relevant.
//
// The two tools match the question identically on use cases, so the only thing
// that can separate them is the anti-example — and the lookalike is declared
// first, so it wins on the order tiebreaker if the penalty does nothing. An
// earlier version of this test let fetch_url win on use-case weight alone and
// would have passed with the penalty removed entirely.
func TestAntiExampleDemotesALookalike(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("web_search", use("抓取网页内容"), anti("抓取网页内容时已知网址")),
		meta("fetch_url", use("抓取网页内容")),
	}
	r := Select("抓取网页内容", tools, Config{})
	if got := topRanked(r); got != "fetch_url" {
		t.Errorf("top = %q, want fetch_url — 反例应压制看起来相关的那个；候选 %+v", got, r.Candidates)
	}
}

// A tool's own name was chosen to say what it is; a description is prose that
// may mention anything. Naming the tool in the question is the most deliberate
// signal there is, and must outrank an incidental mention in prose.
//
// The description-matching tool is declared first, so equal weights would let
// it win on the order tiebreaker.
func TestNameOutranksAnIncidentalMentionInProse(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("restart_service", desc("重启服务，注意可能影响 logs 的连续性")),
		meta("logs", desc("与本次问题无关的说明文字")),
	}
	r := Select("logs", tools, Config{})
	if got := topRanked(r); got != "logs" {
		t.Errorf("top = %q, want logs — 名称命中应重于描述里的顺带提及；候选 %+v", got, r.Candidates)
	}
}

// Below the cap nothing is dropped. This is what makes selection safe to turn
// on: a deployment with few tools behaves exactly as it did before.
func TestBelowTheCapNothingIsDropped(t *testing.T) {
	tools := []ops.ToolMetadata{meta("a"), meta("b"), meta("c")}
	r := Select("完全无关的问题", tools, Config{MaxTools: 10})
	if len(r.Selected) != 3 {
		t.Errorf("selected %d of 3", len(r.Selected))
	}
	if r.Capped {
		t.Error("Capped set although nothing was removed")
	}
	for _, c := range r.Candidates {
		if c.Suppressed != "" {
			t.Errorf("%s suppressed below the cap: %s", c.Tool, c.Suppressed)
		}
	}
}

// Zero means no cap, which is the behaviour before selection existed.
func TestZeroCapKeepsEverything(t *testing.T) {
	tools := make([]ops.ToolMetadata, 40)
	for i := range tools {
		tools[i] = meta(string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}
	if r := Select("x", tools, Config{}); len(r.Selected) != 40 {
		t.Errorf("selected %d of 40 with no cap", len(r.Selected))
	}
}

// Fallback exists so a narrow shortlist cannot strand the model with nothing
// generic — it must beat tools that matched nothing.
func TestFallbackSurvivesAgainstUnmatchedTools(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("noise_1"), meta("noise_2"), meta("noise_3"),
		meta("load_memory", fallback),
	}
	r := Select("一个谁都匹配不上的问题", tools, Config{MaxTools: 2})
	if rankOf(r, "load_memory") < 0 {
		t.Errorf("fallback tool was cut: selected %v", r.Selected)
	}
}

// ...but it must not outrank a tool that actually answers the question. The
// obvious arrangement — fallback above everything — gets this backwards.
func TestFallbackDoesNotDisplaceAMatch(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("load_memory", fallback),
		meta("get_logs", use("查看服务日志")),
	}
	r := Select("看日志", tools, Config{MaxTools: 1})
	if len(r.Selected) != 1 || r.Selected[0] != "get_logs" {
		t.Errorf("selected %v, want the matching tool to win the single slot", r.Selected)
	}
}

// Breaking a stage that depends on a baseline tool is not comparable to
// exceeding a token target.
func TestBaselineOverrunsTheBudgetRatherThanBeingDropped(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("hit", use("查看日志")),
		meta("collect_metrics", baseline),
	}
	r := Select("看日志", tools, Config{MaxTools: 1})
	if rankOf(r, "collect_metrics") < 0 {
		t.Errorf("baseline tool dropped: %v", r.Selected)
	}
	if rankOf(r, "hit") < 0 {
		t.Errorf("the matching tool was displaced by the baseline: %v", r.Selected)
	}
}

// A cut tool must be answerable for. "The model never looked at X" is a dead
// end unless the record says whether X was a candidate and at what score.
func TestCutToolsAreRecordedWithAReason(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("hit", use("查看日志")),
		meta("miss_1"), meta("miss_2"),
	}
	r := Select("看日志", tools, Config{MaxTools: 1})
	if !r.Capped {
		t.Fatal("Capped not set although tools were removed")
	}
	if len(r.Candidates) != 3 {
		t.Fatalf("candidates = %d, want all three considered", len(r.Candidates))
	}
	var suppressed int
	for _, c := range r.Candidates {
		if c.Suppressed != "" {
			suppressed++
		}
	}
	if suppressed != 2 {
		t.Errorf("%d candidates carry a suppression reason, want 2", suppressed)
	}
}

// An identical catalogue and question must produce an identical prompt, or a
// changed answer could come from map iteration rather than from anything real.
func TestSelectionIsDeterministic(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("a", tags("x")), meta("b", tags("x")), meta("c", tags("x")),
		meta("d", tags("x")), meta("e", tags("x")),
	}
	first := Select("x", tools, Config{MaxTools: 3}).Selected
	for i := 0; i < 20; i++ {
		if got := Select("x", tools, Config{MaxTools: 3}).Selected; !slices.Equal(got, first) {
			t.Fatalf("run %d gave %v, first gave %v", i, got, first)
		}
	}
}

// No question is not a reason to guess: the declared order stands and the cap
// still applies, which keeps the prompt bounded rather than relevant.
func TestEmptyQueryFallsBackToDeclaredOrder(t *testing.T) {
	tools := []ops.ToolMetadata{meta("a"), meta("b"), meta("c")}
	r := Select("", tools, Config{MaxTools: 2})
	if !slices.Equal(r.Selected, []string{"a", "b"}) {
		t.Errorf("selected %v, want the first two in declared order", r.Selected)
	}
}

// Being quick is not evidence of being relevant.
//
// The latency tiebreaker used to go into the same total the tiering read, so a
// fast tool that matched nothing scored above zero, counted as a match, and
// outranked the fallback tools that exist for exactly this case. A real run
// showed it: on "你好，一句话介绍你自己" the budget went to remember and
// forget — both fast, both irrelevant — and load_memory came last.
func TestSpeedDoesNotCountAsRelevance(t *testing.T) {
	fast := func(m *ops.ToolMetadata) { m.Latency = ops.LatencyFast }
	tools := []ops.ToolMetadata{
		meta("remember", fast, use("记录用户偏好")),
		meta("forget", fast, use("信息过时")),
		meta("load_memory", fast, fallback, use("回忆历史对话")),
	}
	r := Select("你好，一句话介绍你自己", tools, Config{MaxTools: 1})
	if len(r.Selected) != 1 || r.Selected[0] != "load_memory" {
		t.Errorf("selected %v, want the fallback — nothing matched, so speed must not decide", r.Selected)
	}
}

// The tiebreaker still has to work where it belongs: inside a tier.
func TestLatencyBreaksTiesAmongEqualMatches(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("slow_logs", use("查看日志"), func(m *ops.ToolMetadata) { m.Latency = ops.LatencySlow }),
		meta("fast_logs", use("查看日志"), func(m *ops.ToolMetadata) { m.Latency = ops.LatencyFast }),
	}
	r := Select("查看日志", tools, Config{})
	if topRanked(r) != "fast_logs" {
		t.Errorf("top = %q, want fast_logs — equal matches should prefer the cheaper one", topRanked(r))
	}
}

// An anti-example that fires harder than the positive match is the metadata
// author saying "this looks relevant and is not" — which is the entire reason
// the field exists. Tiering on "matched anything at all" left such a tool in
// the matched tier at −18 points, where it still beat an unmatched fallback at
// 0 and took the last budget slot.
func TestAntiExamplePushesAToolOutOfTheMatchedTier(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("dev_logs", desc("查询日志"), anti("查询生产日志")),
		meta("load_memory", fallback),
	}
	r := Select("查询生产日志", tools, Config{MaxTools: 1})
	if len(r.Selected) != 1 || r.Selected[0] != "load_memory" {
		t.Errorf("selected %v, want the fallback; 候选 %+v", r.Selected, r.Candidates)
	}
	if s := scoreOf(r, "dev_logs"); s >= 0 {
		t.Errorf("dev_logs scored %v, want the anti-example to take it negative", s)
	}
}

// A positive match must still survive an anti-example that only grazes it.
func TestAWeakAntiExampleDoesNotUnseatAStrongMatch(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("get_logs", use("查询日志", "查看容器日志"), anti("查询配置")),
		meta("load_memory", fallback),
	}
	r := Select("查询日志", tools, Config{MaxTools: 1})
	if len(r.Selected) != 1 || r.Selected[0] != "get_logs" {
		t.Errorf("selected %v, want get_logs; 候选 %+v", r.Selected, r.Candidates)
	}
}

// Below the budget, selection must not touch the prompt at all — including the
// order of it. Ranking decides which tools go, never how they are laid out: a
// reordered tool block misses a provider's prefix cache on a prompt that is
// otherwise identical, and a deployment under its budget is supposed to see no
// change whatsoever.
func TestOrderIsStableAcrossQuestions(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("web_search", desc("搜索互联网获取实时信息")),
		meta("fetch_url", desc("抓取指定网址的正文")),
		meta("remember", desc("把值得跨会话记住的事实写入长期记忆")),
	}
	want := []string{"web_search", "fetch_url", "remember"}
	for _, q := range []string{"抓取网址", "记住这件事", "搜一下", ""} {
		r := Select(q, tools, Config{})
		if r.Capped {
			t.Fatalf("%q: 前提不成立，本应未裁剪", q)
		}
		if !slices.Equal(r.Selected, want) {
			t.Errorf("%q: selected %v, want the declared order %v", q, r.Selected, want)
		}
	}
}

// The same holds once the budget bites: two questions that keep the same tools
// must produce the same prompt.
func TestSurvivorsKeepDeclaredOrderWhenCapped(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("a_logs", use("查看日志")),
		meta("b_noise"),
		meta("c_metrics", use("查看指标")),
	}
	r := Select("查看日志和指标", tools, Config{MaxTools: 2})
	if !slices.Equal(r.Selected, []string{"a_logs", "c_metrics"}) {
		t.Errorf("selected %v, want the two survivors in declared order", r.Selected)
	}
	// Ranking is still available, and still ranked.
	if topRanked(r) == "b_noise" {
		t.Errorf("ranking lost: %+v", r.Candidates)
	}
}

// Every candidate must carry what it takes to answer "why did A win over B" —
// both sides' scores and the fields each matched on, not only the losers'.
func TestCandidatesExplainBothSides(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta("winner", use("查看日志")),
		meta("loser", desc("顺带提到日志")),
	}
	r := Select("查看日志", tools, Config{MaxTools: 1})
	for _, name := range []string{"winner", "loser"} {
		c := candidateFor(r, name)
		if c == nil {
			t.Fatalf("%s missing from the candidate record", name)
		}
		if c.Reason == "" {
			t.Errorf("%s carries no reason; 'why did A beat B' is unanswerable", name)
		}
		if c.Score == 0 {
			t.Errorf("%s carries no score", name)
		}
	}
	if a, b := scoreOf(r, "winner"), scoreOf(r, "loser"); a <= b {
		t.Errorf("winner scored %v, loser %v", a, b)
	}
}

func candidateFor(r Result, tool string) *ops.Candidate {
	for i := range r.Candidates {
		if r.Candidates[i].Tool == tool {
			return &r.Candidates[i]
		}
	}
	return nil
}

func scoreOf(r Result, tool string) float64 {
	if c := candidateFor(r, tool); c != nil {
		return c.Score
	}
	return 0
}
