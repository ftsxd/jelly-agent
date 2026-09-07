package selector

// What the question is asking for, when the tools cannot say it in the same
// language.
//
// This exists because of a failure with a very clean shape. A third-party MCP
// server ships English names and English prose and nothing else — no use
// cases, no examples, no tags — while the operators asking the questions write
// Chinese. The two token sets never intersect, so every one of that server's
// tools scores exactly zero, and ranking degenerates into the last tiebreaker,
// which is declaration order. Registration sorts by key, so the tools whose
// names sort last are cut first, every time, for every question. In one real
// deployment that meant the three PromQL query tools — the only ones that
// could have answered a metrics question — were the deterministic casualties
// of a 48-tool budget, and the model spent nine calls and three turns
// concluding that the capability did not exist.
//
// Two signals are recovered here, both weaker than a literal match and both
// deliberately dull:
//
//   - Kind. What the question wants back — a curve, a log excerpt, an alert.
//     Compared against ToolMetadata.Produces, which is the one field an
//     operator can realistically declare for somebody else's tool, and the one
//     the console offers a dropdown for.
//   - Stems. The English words a tool that answers this question would
//     plausibly have in its name or description. "查询 cpu 使用率" implies
//     "query", "promql", "metric" — none of which appear in the question.
//
// The table is the whole risk. A wrong row does not fail loudly; it quietly
// promotes the wrong tool. So entries are added only where the mapping holds
// for any deployment, not where it happens to fit this one: 日志 means logs
// everywhere, whereas a product name does not.

import "github.com/jelly-agent/jelly-agent/internal/ops"

// Weights for the recovered signals.
//
// Two properties, and the second was wrong at first.
//
// Each signal fires once, not once per matching token. The literal scorer
// multiplies its weight by the number of overlapping tokens, which is right
// for testimony — use cases matching the question in four places really is a
// better fit than matching in one. It is wrong for inference: "this name looks
// like it does metrics" is a single observation however many plausible words
// the name happens to contain, and scaling it meant a tool named
// query_range_promql_metric_usage scored 16.0 against a literal name match's
// 12.0. Inference outranking testimony was exactly what this must not do.
//
// And the group is bounded below wUseCase, so any deliberate declaration — a
// name the question used, a use case somebody wrote down — outranks any amount
// of inference. Inference still beats an incidental mention in prose
// (wDescription = 1.0) and beats no signal at all, which is the whole job.
const (
	wKind         = 1.2 // question's evident kind == m.Produces
	wStemName     = 1.2 // an implied English stem appears in the tool's name
	wStemDescribe = 0.5 // ... or in its description

	// maxInferred is what all three together can reach. A test asserts it
	// against wUseCase, so the bound is a checked property rather than an
	// arithmetic coincidence that survives until somebody bumps a weight.
	maxInferred = wKind + wStemName + wStemDescribe
)

// intent is what a question appears to be asking for.
type intent struct {
	kinds map[ops.EvidenceKind]bool
	stems map[string]bool
}

func (i intent) empty() bool { return len(i.kinds) == 0 && len(i.stems) == 0 }

// cue maps a question token to what it implies.
//
// A Chinese key must be exactly two characters, because that is what tokenize
// produces for CJK: "使用率" arrives as 使用 and 用率, and "日志检索" as 日志、
// 志检、检索 — never as one word. A four-character key is not a strict entry,
// it is a dead one, and a test refuses to let another be added. Latin keys are
// whole words, lowercased.
type cue struct {
	kinds []ops.EvidenceKind
	stems []string
}

// cues is the lexicon. Every row is a claim that holds regardless of which
// monitoring stack is deployed.
var cues = map[string]cue{
	// ── 指标 / 时序 ────────────────────────────────────────────────
	"指标": {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query", "promql"}},
	"监控": {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query", "promql"}},
	"曲线": {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"query", "range", "promql"}},
	"时序": {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"query", "range", "promql"}},
	// 用率 and not 使用: the bigram 使用 also sits in 怎么使用、使用说明、该使用
	// 哪个数据源, none of which is a metrics question, and it made all three
	// infer metric_series. 用率 is the discriminating half of 使用率 and 利用率
	// alike.
	"用率":      {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query", "usage"}},
	"水位":      {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query"}},
	"负载":      {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query", "load"}},
	"延迟":      {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query", "latency"}},
	"耗时":      {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query", "latency"}},
	"内存":      {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query", "memory"}},
	"cpu":     {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query", "promql"}},
	"mem":     {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query", "memory"}},
	"memory":  {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query"}},
	"qps":     {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query", "promql"}},
	"latency": {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query"}},
	"usage":   {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query"}},
	"util":    {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"metric", "query"}},
	"metric":  {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"query", "promql"}},
	"metrics": {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"query", "promql"}},
	"promql":  {[]ops.EvidenceKind{ops.KindMetricSeries}, []string{"query", "promql", "range", "instant"}},

	// ── 日志 ──────────────────────────────────────────────────────
	"日志":        {[]ops.EvidenceKind{ops.KindLogExcerpt}, []string{"log", "logs", "query"}},
	"报错":        {[]ops.EvidenceKind{ops.KindLogExcerpt}, []string{"log", "logs", "error"}},
	"堆栈":        {[]ops.EvidenceKind{ops.KindLogExcerpt}, []string{"log", "logs", "trace"}},
	"log":       {[]ops.EvidenceKind{ops.KindLogExcerpt}, []string{"log", "logs", "query"}},
	"logs":      {[]ops.EvidenceKind{ops.KindLogExcerpt}, []string{"log", "logs", "query"}},
	"traceback": {[]ops.EvidenceKind{ops.KindLogExcerpt}, []string{"log", "trace"}},

	// ── 告警 / 事件 ───────────────────────────────────────────────
	"告警":     {[]ops.EvidenceKind{ops.KindEvents}, []string{"alert", "alerts", "alarm"}},
	"报警":     {[]ops.EvidenceKind{ops.KindEvents}, []string{"alert", "alerts", "alarm"}},
	"事件":     {[]ops.EvidenceKind{ops.KindEvents}, []string{"event", "events"}},
	"alert":  {[]ops.EvidenceKind{ops.KindEvents}, []string{"alert", "alerts"}},
	"alerts": {[]ops.EvidenceKind{ops.KindEvents}, []string{"alert", "alerts"}},
	"alarm":  {[]ops.EvidenceKind{ops.KindEvents}, []string{"alert", "alarm"}},

	// ── 运行状态 ──────────────────────────────────────────────────
	"状态":   {[]ops.EvidenceKind{ops.KindWorkloadStatus}, []string{"status", "target", "targets"}},
	"实例":   {[]ops.EvidenceKind{ops.KindWorkloadStatus}, []string{"instance", "target", "targets"}},
	"副本":   {[]ops.EvidenceKind{ops.KindWorkloadStatus}, []string{"replica", "pod", "pods"}},
	"重启":   {[]ops.EvidenceKind{ops.KindWorkloadStatus}, []string{"restart", "pod", "pods"}},
	"存活":   {[]ops.EvidenceKind{ops.KindWorkloadStatus}, []string{"status", "up", "target"}},
	"pod":  {[]ops.EvidenceKind{ops.KindWorkloadStatus}, []string{"pod", "pods"}},
	"pods": {[]ops.EvidenceKind{ops.KindWorkloadStatus}, []string{"pod", "pods"}},
	"节点":   {[]ops.EvidenceKind{ops.KindWorkloadStatus}, []string{"node", "nodes", "target"}},

	// ── 配置 / 拓扑 ───────────────────────────────────────────────
	"配置":       {[]ops.EvidenceKind{ops.KindConfig}, []string{"config", "get"}},
	"config":   {[]ops.EvidenceKind{ops.KindConfig}, []string{"config"}},
	"拓扑":       {[]ops.EvidenceKind{ops.KindTopology}, []string{"topology", "route"}},
	"依赖":       {[]ops.EvidenceKind{ops.KindTopology}, []string{"topology", "dependency"}},
	"topology": {[]ops.EvidenceKind{ops.KindTopology}, []string{"topology"}},
}

// inferIntent reads the question's tokens through the lexicon.
func inferIntent(q map[string]bool) intent {
	in := intent{kinds: map[ops.EvidenceKind]bool{}, stems: map[string]bool{}}
	for tok := range q {
		c, ok := cues[tok]
		if !ok {
			continue
		}
		for _, k := range c.kinds {
			in.kinds[k] = true
		}
		for _, s := range c.stems {
			// Never re-add a word the question already used: that would count
			// the same evidence twice, once as literal and once as inferred.
			if !q[s] {
				in.stems[s] = true
			}
		}
	}
	return in
}
