package server

// What ADK's timestamps and event boundaries actually mean.
//
// The frames this package derives — a round identity, an elapsed time — are
// not fields ADK hands over; they are inferences about how it groups events.
// Inferences about somebody else's framework have to be checked against the
// framework, so these tests drive a real llmagent through a real Runner with a
// scripted model, and assert on the events that come out.
//
// Three of them exist because the first version of the projector guessed
// wrong: partial text got a hardcoded round number, the round counter meant
// different things live and on replay, and the elapsed time was read off a
// timestamp that ADK reuses from an earlier event.

import (
	"context"
	"iter"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	adksession "google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

// scriptedModel replays a fixed list of turns, one per GenerateContent call.
type scriptedModel struct {
	turns [][]*adkmodel.LLMResponse
	calls int
}

func (m *scriptedModel) Name() string { return "scripted" }

func (m *scriptedModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	i := m.calls
	m.calls++
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if i >= len(m.turns) {
			yield(text("（脚本已尽）"), nil)
			return
		}
		for _, r := range m.turns[i] {
			if !yield(r, nil) {
				return
			}
		}
	}
}

func text(s string) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: s}}},
	}
}

func partialText(s string) *adkmodel.LLMResponse {
	r := text(s)
	r.Partial = true
	return r
}

func calls(names ...string) *adkmodel.LLMResponse {
	parts := make([]*genai.Part, 0, len(names))
	for _, n := range names {
		parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{Name: n, Args: map[string]any{}}})
	}
	return &adkmodel.LLMResponse{Content: &genai.Content{Role: "model", Parts: parts}}
}

type noArgs struct{}
type okResult struct {
	OK bool `json:"ok"`
}

// sleeper is a tool that takes a known amount of time, so a claim about
// durations can be checked against a duration the test chose.
func sleeper(t *testing.T, name string, d time.Duration) adktool.Tool {
	t.Helper()
	tl, err := functiontool.New(
		functiontool.Config{Name: name, Description: name},
		func(agent.ToolContext, noArgs) (okResult, error) {
			time.Sleep(d)
			return okResult{OK: true}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

// runScript drives one user message through a real Runner and returns every
// event it produced, in order.
func runScript(t *testing.T, m *scriptedModel, tools []adktool.Tool, sessionID string, messages ...string) []*adksession.Event {
	t.Helper()
	a, err := llmagent.New(llmagent.Config{Name: "root", Model: m, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	svc := adksession.InMemoryService()
	r, err := runner.New(runner.Config{AppName: "test", Agent: a, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: "test", UserID: "u", SessionID: sessionID,
	}); err != nil {
		t.Fatal(err)
	}

	var out []*adksession.Event
	for _, msg := range messages {
		for ev, err := range r.Run(ctx, "u", sessionID,
			genai.NewContentFromText(msg, genai.RoleUser),
			agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) {
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if ev != nil {
				out = append(out, ev)
			}
		}
	}
	return out
}

// Partial and final events of one response must share a round identity, or
// there is no way to attach streamed text to the round it belongs to.
//
// The first version emitted a hardcoded 0 on every delta while the final text
// of the same response carried a counter — so nothing lined up, and the
// comment claiming the reducer could look up a round by it was simply false.
func TestPartialAndFinalShareARoundIdentity(t *testing.T) {
	m := &scriptedModel{turns: [][]*adkmodel.LLMResponse{
		{partialText("副本"), partialText("正常"), text("副本正常")},
	}}
	frames, _ := projectAll(runScript(t, m, nil, "s1", "看下副本"))

	deltas := only(frames, frameTextDelta)
	finals := only(frames, frameText)
	if len(deltas) == 0 || len(finals) == 0 {
		t.Fatalf("frames = %v, want both deltas and a final text", typesOf(frames))
	}
	for _, d := range deltas {
		if d["round"] != finals[0]["round"] {
			t.Fatalf("delta round = %v, final round = %v; the two cannot be folded together",
				d["round"], finals[0]["round"])
		}
	}
	if finals[0]["round"] == "" || finals[0]["round"] == nil {
		t.Error("round identity is empty")
	}
}

// The identity must mean the same thing live and on replay.
//
// Live rebuilds its state per request; replay walks the whole stored session.
// A counter therefore numbered the same answer differently in the two paths —
// continuing a history session would collide, and a refresh would renumber
// everything.
func TestRoundIdentityIsTheSameLiveAndOnReplay(t *testing.T) {
	script := func() *scriptedModel {
		return &scriptedModel{turns: [][]*adkmodel.LLMResponse{
			{text("第一个回答")},
			{text("第二个回答")},
		}}
	}
	// Replay: one projector over the whole session.
	all := runScript(t, script(), nil, "s1", "问题一", "问题二")
	replay, _ := projectAll(all)

	// Live: a fresh projector per request, which is what the handler does.
	var live []map[string]any
	events := runScript(t, script(), nil, "s2", "问题一", "问题二")
	perTurn := splitByInvocation(events)
	for _, turn := range perTurn {
		c := &collector{}
		st := newTurnState()
		for _, ev := range turn {
			project(ev, c, st)
		}
		live = append(live, c.frames...)
	}

	replayRounds := roundsOf(only(replay, frameText))
	liveRounds := roundsOf(only(live, frameText))
	if len(replayRounds) != len(liveRounds) || len(replayRounds) == 0 {
		t.Fatalf("replay had %d text frames, live had %d", len(replayRounds), len(liveRounds))
	}
	// The values differ between sessions (they are ids), but within one path
	// the two answers must be distinct, and neither path may renumber.
	if replayRounds[0] == replayRounds[1] {
		t.Error("replay gave both answers the same round identity")
	}
	if liveRounds[0] == liveRounds[1] {
		t.Error("live gave both answers the same round identity")
	}
}

func roundsOf(frames []map[string]any) []any {
	out := make([]any, 0, len(frames))
	for _, f := range frames {
		out = append(out, f["round"])
	}
	return out
}

// splitByInvocation groups events the way separate HTTP requests would see
// them: one group per invocation.
func splitByInvocation(events []*adksession.Event) [][]*adksession.Event {
	var out [][]*adksession.Event
	cur := ""
	for _, ev := range events {
		if ev.InvocationID != cur {
			cur = ev.InvocationID
			out = append(out, nil)
		}
		out[len(out)-1] = append(out[len(out)-1], ev)
	}
	return out
}

// ADK waits for every parallel tool, then emits one merged response event that
// reuses the *first* tool's event — timestamp included
// (base_flow.go:1346 "reuse events[0]"). So the merged event's timestamp is
// neither when the tools finished nor when the frame was delivered, and any
// elapsed time derived from it describes nothing.
//
// This is why tool frames carry no elapsed time at all until real per-call
// timing is wired in: a number that looks like a duration and is not one is
// worse than no number, because nobody re-checks a number that renders.
func TestParallelToolsGetNoInventedDuration(t *testing.T) {
	tools := []adktool.Tool{
		sleeper(t, "fast", 0),
		sleeper(t, "slow", 100*time.Millisecond),
	}
	m := &scriptedModel{turns: [][]*adkmodel.LLMResponse{
		{calls("fast", "slow")},
		{text("都好了")},
	}}
	frames, _ := projectAll(runScript(t, m, tools, "s1", "并行跑一下"))

	results := only(frames, frameToolResult)
	if len(results) != 2 {
		t.Fatalf("tool results = %d, want 2: %v", len(results), typesOf(frames))
	}
	for _, r := range results {
		if v, present := r["elapsed_ms"]; present {
			t.Errorf("%v reported elapsed_ms=%v, which ADK's merged timestamp cannot support",
				r["name"], v)
		}
	}
}

// The rest of the projection has to keep working on real events, not just on
// hand-built ones: pairing by call id, and one round per model response.
func TestRealRunPairsCallsAndCountsRounds(t *testing.T) {
	tools := []adktool.Tool{sleeper(t, "check", 0)}
	m := &scriptedModel{turns: [][]*adkmodel.LLMResponse{
		{calls("check")},
		{text("检查通过")},
	}}
	frames, usage := projectAll(runScript(t, m, tools, "s1", "检查一下"))

	call := only(frames, frameToolCall)
	res := only(frames, frameToolResult)
	if len(call) != 1 || len(res) != 1 {
		t.Fatalf("frames = %v, want one call and one result", typesOf(frames))
	}
	if call[0]["call_id"] == "" || call[0]["call_id"] == nil {
		t.Fatal("ADK produced a call with no id; pairing depends on it")
	}
	if call[0]["call_id"] != res[0]["call_id"] {
		t.Errorf("call %v and result %v do not share an id", call[0]["call_id"], res[0]["call_id"])
	}
	// Two model responses, so two rounds — the tool response in between must
	// not open one of its own.
	if n := len(only(frames, frameLLMTurn)); n != 0 && n != 2 {
		t.Errorf("llm_turn frames = %d, want 0 (no usage from the scripted model) or 2", n)
	}
	_ = usage
}
