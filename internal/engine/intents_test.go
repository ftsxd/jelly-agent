package engine

import (
	"slices"
	"testing"

	adktool "google.golang.org/adk/tool"

	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/selector"
)

// Intent accumulates within a conversation and stays out of other people's.
func TestIntentAccumulatesPerSession(t *testing.T) {
	r := newIntents()

	first := r.carry("web-1", selector.Infer("查一下 cpu 使用率"))
	if first.Empty() {
		t.Fatal("第一轮就该推断出指标意图")
	}
	// The follow-up says nothing on its own; what comes back is the union.
	got := r.carry("web-1", selector.Infer("集群 id 我不清楚，你给我下"))
	if got.Empty() {
		t.Error("追问丢掉了这次对话累积的意图")
	}

	// Another conversation starts clean.
	other := r.carry("web-2", selector.Infer("集群 id 我不清楚，你给我下"))
	if !other.Empty() {
		t.Errorf("别的会话串进来了: %+v", other)
	}
}

// A first turn behaves exactly as it did before this existed.
func TestAFirstTurnCarriesNothingExtra(t *testing.T) {
	r := newIntents()
	q := "看下日志"
	if got, want := r.carry("web-1", selector.Infer(q)), selector.Infer(q); got.Empty() != want.Empty() {
		t.Errorf("第一轮的意图被改动了")
	}
	// And with no session there is nothing to be continuous across.
	if got := r.carry("", selector.Infer("集群 id 我不清楚")); !got.Empty() {
		t.Errorf("没有会话时不该有累积: %+v", got)
	}
}

// The record is bounded, and evicting one costs that conversation its history
// rather than corrupting another's.
func TestTheIntentRecordIsBounded(t *testing.T) {
	r := newIntents()
	for i := 0; i < maxIntentSessions+10; i++ {
		r.carry(sessionName(i), selector.Infer("cpu 使用率"))
	}
	if len(r.byID) > maxIntentSessions {
		t.Errorf("记录了 %d 个会话，上限是 %d", len(r.byID), maxIntentSessions)
	}
	// The newest is still there.
	last := sessionName(maxIntentSessions + 9)
	if _, ok := r.byID[last]; !ok {
		t.Error("最近的会话被挤掉了")
	}
}

func sessionName(i int) string {
	return "web-" + string(rune('a'+i%26)) + "-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// Belt and braces: the kinds a carried intent holds are the ones Select reads.
func TestCarriedIntentReachesSelection(t *testing.T) {
	tools := []ops.ToolMetadata{
		meta2("query_range", "Run a PromQL range query."),
		meta2("list_users", "List users."),
	}
	r := newIntents()
	r.carry("web-1", selector.Infer("cpu 使用率"))
	carried := r.carry("web-1", selector.Infer("再看看"))

	res := selector.Select("再看看", tools, selector.Config{Carried: carried})
	if res.Candidates[0].Tool != "query_range" {
		t.Errorf("排最前的是 %q，累积的指标意图没有传到选择里", res.Candidates[0].Tool)
	}
}

func meta2(name, description string) ops.ToolMetadata {
	return ops.ToolMetadata{Name: name, Description: description}
}

// Through the toolset a turn actually calls, not through Select directly.
//
// "累积意图有效" and "这条路径用上了它" are different claims, and only the
// second one is what a conversation depends on. A test of Select alone stays
// green the day the call site stops passing it — which is exactly the shape of
// the bug being fixed here, one layer down.
func TestTheToolsetCarriesIntentAcrossTurns(t *testing.T) {
	// A field of zero-scoring tools that sort before query_*, so the budget
	// cuts the query tools unless something promotes them.
	var static []adktool.Tool
	for _, n := range []string{
		"get_notify_channel", "list_alert_subscribes", "list_mutes", "list_roles",
	} {
		static = append(static, &stubTool{name: n})
	}
	static = append(static,
		&stubTool{name: "query_range", desc: "Run a PromQL range query against a Prometheus-compatible datasource."})

	// admit is deliberately nil. Admission would keep the tool on turn two all
	// by itself — it entered the prompt on turn one — so with it on, this test
	// cannot tell which mechanism did the work, and passed even with the
	// carried intent unplugged. Off, only the intent can explain the result.
	sel := &selectingToolset{
		static:  static,
		cfg:     selector.Config{MaxTools: 4},
		carried: newIntents(),
	}
	names := func(ts []adktool.Tool) []string {
		out := make([]string, 0, len(ts))
		for _, x := range ts {
			out = append(out, x.Name())
		}
		return out
	}
	has := func(ts []adktool.Tool, want string) bool {
		return slices.Contains(names(ts), want)
	}

	// Turn one names the topic.
	first, err := sel.Tools(&askingCtx{session: "web-1", question: "查一下这个实例最近 3 天的 cpu 使用率"})
	if err != nil {
		t.Fatal(err)
	}
	if !has(first, "query_range") {
		t.Fatalf("第一轮就没选上 query_range: %v", names(first))
	}

	// Turn two does not name it, and must not lose it.
	second, err := sel.Tools(&askingCtx{session: "web-1", question: "集群 id 我不清楚，你给我下"})
	if err != nil {
		t.Fatal(err)
	}
	if !has(second, "query_range") {
		t.Errorf("追问把查询工具弄丢了: %v", names(second))
	}

	// A conversation that never named the topic does not get it — the record
	// is per session, not global.
	other, err := sel.Tools(&askingCtx{session: "web-2", question: "集群 id 我不清楚，你给我下"})
	if err != nil {
		t.Fatal(err)
	}
	if has(other, "query_range") {
		t.Errorf("别的会话的意图串过来了: %v", names(other))
	}
}
