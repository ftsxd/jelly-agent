package server

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adksession "google.golang.org/adk/session"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/engine"
)

var base = time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)

// at builds a timestamp n milliseconds into the turn, so elapsed times in the
// assertions are the numbers the test wrote rather than wall-clock noise.
func at(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }

type evOpt func(*adksession.Event)

func author(a string) evOpt       { return func(e *adksession.Event) { e.Author = a } }
func branch(b string) evOpt       { return func(e *adksession.Event) { e.Branch = b } }
func partial(e *adksession.Event) { e.Partial = true }
func transferTo(a string) evOpt {
	return func(e *adksession.Event) { e.Actions.TransferToAgent = a }
}
func usage(prompt, completion, total int32) evOpt {
	return func(e *adksession.Event) {
		e.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount: prompt, CandidatesTokenCount: completion, TotalTokenCount: total,
		}
	}
}

// evID gives each constructed event a distinct id. The store's primary key
// includes it, so events built without one collide the moment a test writes
// more than one.
var evID atomic.Uint64

func event(ts time.Time, role string, parts []*genai.Part, opts ...evOpt) *adksession.Event {
	e := &adksession.Event{
		ID: "ev-" + strconv.FormatUint(evID.Add(1), 10), Timestamp: ts, Author: "root",
	}
	if parts != nil || role != "" {
		e.Content = &genai.Content{Role: role, Parts: parts}
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

func callPart(id, name string, args map[string]any) *genai.Part {
	return &genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}}
}

func respPart(id, name string, resp map[string]any) *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: id, Name: name, Response: resp}}
}

func textPart(s string) *genai.Part    { return &genai.Part{Text: s} }
func thoughtPart(s string) *genai.Part { return &genai.Part{Text: s, Thought: true} }

// run projects a list of events and returns the frames plus the accumulated
// usage, the same way the replay endpoint will.
func run(events ...*adksession.Event) ([]map[string]any, map[string]any) {
	return projectAll(events)
}

func typesOf(frames []map[string]any) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f["type"].(string))
	}
	return out
}

// only returns the frames of one type.
func only(frames []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, f := range frames {
		if f["type"] == typ {
			out = append(out, f)
		}
	}
	return out
}

func count(frames []map[string]any, typ string) int { return len(only(frames, typ)) }

// Two calls to the same tool, answered out of order, must each keep their own
// arguments. The frontend pairs by name today and searches backwards, so on
// this input it hands the second result to the first call — the UI then shows
// a call that never happened, with one call's arguments and another's result.
func TestConcurrentSameNameCallsPairByID(t *testing.T) {
	frames, _ := run(
		event(at(0), "model", []*genai.Part{
			callPart("c1", "get_pods", map[string]any{"ns": "prod"}),
			callPart("c2", "get_pods", map[string]any{"ns": "staging"}),
		}),
		// Answered in the opposite order.
		event(at(30), "user", []*genai.Part{respPart("c2", "get_pods", map[string]any{"n": 2})}),
		event(at(50), "user", []*genai.Part{respPart("c1", "get_pods", map[string]any{"n": 1})}),
	)

	// The projector's part of pairing is to put the right id on both sides.
	// The fold itself is the reducer's, and it is only possible because these
	// ids are here and distinct.
	argsByID := map[any]any{}
	for _, c := range only(frames, frameToolCall) {
		argsByID[c["call_id"]] = c["args"]
	}
	if len(argsByID) != 2 {
		t.Fatalf("calls carried %d distinct ids, want 2", len(argsByID))
	}
	results := only(frames, frameToolResult)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2: %v", len(results), typesOf(frames))
	}
	seen := map[any]bool{}
	for _, r := range results {
		id := r["call_id"]
		if _, ok := argsByID[id]; !ok {
			t.Errorf("result carries id %v, which no call used", id)
		}
		if seen[id] {
			t.Errorf("two results claimed id %v", id)
		}
		seen[id] = true
	}
	// The arguments stayed with their own call. Pairing by name would have
	// swapped these, which is the bug this replaces.
	if got := argsByID["c1"]; got == nil || got.(map[string]any)["ns"] != "prod" {
		t.Errorf("c1 args = %v, want ns=prod", got)
	}
	if got := argsByID["c2"]; got == nil || got.(map[string]any)["ns"] != "staging" {
		t.Errorf("c2 args = %v, want ns=staging", got)
	}
}

// A handoff is not work. It arrives on Actions, which the old event loop threw
// away entirely, and it also arrives as an ordinary tool call, which would
// make "this turn called five tools" untrue.
func TestTransferBecomesItsOwnFrameAndNotATool(t *testing.T) {
	frames, _ := run(
		event(at(0), "model",
			[]*genai.Part{callPart("t1", transferToAgentTool, map[string]any{"agent_name": "k8s"})},
			transferTo("k8s")),
		event(at(10), "user", []*genai.Part{respPart("t1", transferToAgentTool, map[string]any{})}),
	)

	if n := count(frames, frameAgentTransfer); n != 1 {
		t.Errorf("agent_transfer frames = %d, want 1: %v", n, typesOf(frames))
	}
	if n := count(frames, frameToolCall); n != 0 {
		t.Errorf("transfer leaked as a tool call: %v", typesOf(frames))
	}
	if n := count(frames, frameToolResult); n != 0 {
		t.Errorf("transfer leaked as a tool result: %v", typesOf(frames))
	}
	tr := only(frames, frameAgentTransfer)[0]
	if tr["from"] != "root" || tr["to"] != "k8s" {
		t.Errorf("transfer = %v, want root → k8s", tr)
	}
}

// An event carrying only Actions has no Content. The old loop skipped those,
// which is exactly how transfers became invisible.
func TestActionsOnlyEventIsNotDropped(t *testing.T) {
	frames, _ := run(event(at(0), "", nil, transferTo("k8s")))
	if n := count(frames, frameAgentTransfer); n != 1 {
		t.Fatalf("frames = %v, want an agent_transfer", typesOf(frames))
	}
}

// Reasoning is not the answer. It has to be separable, or it lands in the
// reply bubble.
func TestThoughtIsSeparateFromText(t *testing.T) {
	frames, _ := run(event(at(0), "model", []*genai.Part{
		thoughtPart("先确认副本数"),
		textPart("副本数正常。"),
	}))

	th := only(frames, frameThought)
	tx := only(frames, frameText)
	if len(th) != 1 || len(tx) != 1 {
		t.Fatalf("thought=%d text=%d, want 1 and 1: %v", len(th), len(tx), typesOf(frames))
	}
	if th[0]["text"] == tx[0]["text"] {
		t.Error("thought and text carry the same content")
	}
	if tx[0]["text"] != "副本数正常。" {
		t.Errorf("text = %v, want the answer only", tx[0]["text"])
	}
}

// Tokens are a property of the turn, not of its last round. The browser
// assigns the usage frame rather than adding it, so a turn with tool rounds
// shows only the final round while the sessions page shows the sum — the same
// session, two pages, two numbers.
func TestUsageAccumulatesAcrossRounds(t *testing.T) {
	_, total := run(
		event(at(0), "model", []*genai.Part{callPart("c1", "get_pods", nil)}, usage(100, 10, 110)),
		event(at(20), "user", []*genai.Part{respPart("c1", "get_pods", map[string]any{"n": 1})}),
		event(at(40), "model", []*genai.Part{textPart("done")}, usage(150, 20, 170)),
	)
	if total["prompt"] != int32(250) || total["completion"] != int32(30) || total["total"] != int32(280) {
		t.Errorf("usage = %v, want the sum of both rounds (250/30/280)", total)
	}
}

// The round counter follows model responses. Function responses come back with
// role "user" and must not open a round, or every tool call would look like a
// separate LLM turn.
func TestRoundCounterFollowsModelResponsesOnly(t *testing.T) {
	frames, _ := run(
		event(at(0), "model", []*genai.Part{callPart("c1", "get_pods", nil)}, usage(1, 1, 2)),
		event(at(10), "user", []*genai.Part{respPart("c1", "get_pods", map[string]any{"n": 1})}),
		event(at(20), "model", []*genai.Part{textPart("ok")}, usage(1, 1, 2)),
	)
	turns := only(frames, frameLLMTurn)
	if len(turns) != 2 {
		t.Fatalf("llm_turn frames = %d, want 2", len(turns))
	}
	if turns[0]["turn"] != 1 || turns[1]["turn"] != 2 {
		t.Errorf("turns = %v, %v; want 1, 2", turns[0]["turn"], turns[1]["turn"])
	}
	// The tool result belongs to turn 1, not to a turn of its own.
	if got := only(frames, frameToolCall)[0]["turn"]; got != 1 {
		t.Errorf("tool call turn = %v, want 1", got)
	}
}

// Success is the server's judgement. A tool that legitimately answers null is
// not still running — which is what the browser concludes today when it
// decides by whether a response is present.
func TestFailureIsJudgedByTheSharedHelper(t *testing.T) {
	frames, _ := run(
		event(at(0), "model", []*genai.Part{
			callPart("c1", "broken", nil),
			callPart("c2", "empty", nil),
		}),
		event(at(10), "user", []*genai.Part{
			respPart("c1", "broken", map[string]any{"error": "connection refused"}),
			respPart("c2", "empty", nil),
		}),
	)
	for _, r := range only(frames, frameToolResult) {
		switch r["call_id"] {
		case "c1":
			if r["ok"] != false || r["error"] != "connection refused" {
				t.Errorf("failed call reported ok=%v error=%v", r["ok"], r["error"])
			}
		case "c2":
			if r["ok"] != true {
				t.Errorf("a tool answering nothing was reported as failed: %v", r)
			}
		}
	}
}

// Streaming text stays lean: one frame per token, so every field is paid
// thousands of times per answer.
func TestPartialTextCarriesOnlyWhatItNeeds(t *testing.T) {
	frames, _ := run(event(at(0), "model", []*genai.Part{textPart("你")}, partial))
	d := only(frames, frameTextDelta)
	if len(d) != 1 {
		t.Fatalf("frames = %v, want one text_delta", typesOf(frames))
	}
	for _, heavy := range []string{"ts", "branch", "invocation_id"} {
		if _, present := d[0][heavy]; present {
			t.Errorf("text_delta carries %q; per-token frames must stay lean", heavy)
		}
	}
}

// A partial event must not be mistaken for a final one, or the answer would be
// emitted twice — once as deltas and once as a whole.
func TestPartialDoesNotAlsoEmitText(t *testing.T) {
	frames, _ := run(event(at(0), "model", []*genai.Part{textPart("hi")}, partial))
	if n := count(frames, frameText); n != 0 {
		t.Errorf("partial event produced a text frame: %v", typesOf(frames))
	}
}

// The whitelist is the point: recognising nothing is the correct response to
// an event shape this code has never seen, because the alternative puts ADK's
// next internal bookkeeping event on the user's screen.
func TestUnrecognisedEventsProduceNothing(t *testing.T) {
	frames, _ := run(
		event(at(0), "model", []*genai.Part{{InlineData: &genai.Blob{MIMEType: "image/png"}}}),
		event(at(10), "model", []*genai.Part{nil}),
		event(at(20), "", nil),
		nil,
	)
	if len(frames) != 0 {
		t.Errorf("frames = %v, want none", typesOf(frames))
	}
}

// Branch is how a sub-agent's steps are nested. Carried on the frames that can
// afford it, and it must survive.
func TestBranchIsCarried(t *testing.T) {
	frames, _ := run(event(at(0), "model",
		[]*genai.Part{callPart("c1", "get_pods", nil)},
		author("k8s"), branch("root.k8s")))
	c := only(frames, frameToolCall)[0]
	if c["branch"] != "root.k8s" || c["agent"] != "k8s" {
		t.Errorf("call = %v, want branch root.k8s and agent k8s", c)
	}
}

// The user's own message is part of the timeline, so replay starts where the
// conversation started rather than at the first model response.
func TestUserMessageIsProjected(t *testing.T) {
	frames, _ := run(event(at(0), "user", []*genai.Part{textPart("看下日志")}))
	u := only(frames, frameUserMessage)
	if len(u) != 1 || u[0]["text"] != "看下日志" {
		t.Fatalf("frames = %v, want a user_message", typesOf(frames))
	}
}

// A confirmation request is not a tool call. Until batch three gives it a
// frame of its own, it must not masquerade as one — the user would see a tool
// named adk_request_confirmation fail.
func TestConfirmationCallIsNotATool(t *testing.T) {
	frames, _ := run(
		event(at(0), "model", []*genai.Part{
			callPart("k1", toolconfirmation.FunctionCallName, map[string]any{}),
		}),
		event(at(10), "user", []*genai.Part{
			respPart("k1", toolconfirmation.FunctionCallName, map[string]any{}),
		}),
	)
	if n := count(frames, frameToolCall) + count(frames, frameToolResult); n != 0 {
		t.Errorf("confirmation surfaced as a tool: %v", typesOf(frames))
	}
}

// Pending approvals come from the stream, not from a table: a granted approval
// is a response sitting next to its request, and a second store could only
// disagree with that.
func TestPendingApprovalsAreDerivedFromTheStream(t *testing.T) {
	events := []*adksession.Event{
		event(at(0), "model", []*genai.Part{callPart("k1", toolconfirmation.FunctionCallName, nil)}),
		event(at(10), "model", []*genai.Part{callPart("k2", toolconfirmation.FunctionCallName, nil)}),
		event(at(20), "user", []*genai.Part{respPart("k1", toolconfirmation.FunctionCallName, map[string]any{"confirmed": true})}),
	}
	got := pendingApprovals(events)
	if len(got) != 1 || got[0] != "k2" {
		t.Errorf("pending = %v, want only k2 — k1 was answered", got)
	}
}

// Replaying the same events twice must give the same frames, or a cached page
// and a fresh one would disagree.
func TestProjectionIsDeterministic(t *testing.T) {
	build := func() []*adksession.Event {
		return []*adksession.Event{
			event(at(0), "model", []*genai.Part{callPart("c1", "a", nil), callPart("c2", "b", nil)}, usage(1, 1, 2)),
			event(at(10), "user", []*genai.Part{respPart("c2", "b", nil), respPart("c1", "a", nil)}),
			event(at(20), "model", []*genai.Part{textPart("ok")}, usage(1, 1, 2)),
		}
	}
	first, _ := projectAll(build())
	for i := 0; i < 10; i++ {
		again, _ := projectAll(build())
		if len(again) != len(first) {
			t.Fatalf("run %d produced %d frames, first produced %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j]["type"] != first[j]["type"] {
				t.Fatalf("run %d frame %d = %v, first = %v", i, j, again[j]["type"], first[j]["type"])
			}
		}
	}
}

// A result with no matching call still has to render — it happens when a
// session is truncated or an old event predates a change.
func TestOrphanResultStillProjects(t *testing.T) {
	frames, _ := run(event(at(10), "user", []*genai.Part{respPart("gone", "get_pods", map[string]any{"n": 1})}))
	r := only(frames, frameToolResult)
	if len(r) != 1 {
		t.Fatalf("frames = %v, want the orphan result", typesOf(frames))
	}
	if _, present := r[0]["elapsed_ms"]; present {
		t.Error("an orphan result reported an elapsed time it cannot know")
	}
}

// The wire format, driven through the real sseWriter.
//
// This is what the seam in streamTurn is for: the whole streaming path runs
// here with no provider, no model and no socket, so the bytes the browser
// actually receives are checked rather than assumed.
func TestStreamTurnWritesRealSSEFrames(t *testing.T) {
	rec := httptest.NewRecorder()
	sse := &sseWriter{w: rec, flusher: rec}

	st, err := streamTurn(sse, seqOf(
		yielded{ev: event(at(0), "model", []*genai.Part{textPart("查")}, partial)},
		yielded{ev: event(at(10), "model", []*genai.Part{callPart("c1", "get_logs", map[string]any{"ns": "prod"})}, usage(90, 8, 98))},
		yielded{ev: event(at(60), "user", []*genai.Part{respPart("c1", "get_logs", map[string]any{"lines": 3})})},
		yielded{ev: event(at(80), "model", []*genai.Part{textPart("没有异常。")}, usage(120, 12, 132))},
	))
	if err != nil {
		t.Fatalf("streamTurn: %v", err)
	}

	frames := parseSSE(t, rec.Body.String())
	got := typesOf(frames)
	want := []string{
		frameTextDelta,
		frameToolCall, frameLLMTurn, frameUsage,
		frameToolResult,
		frameText, frameLLMTurn, frameUsage,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("frames = %v\nwant     %v", got, want)
	}

	// The fact the old stream could not carry.
	call := only(frames, frameToolCall)[0]
	res := only(frames, frameToolResult)[0]
	if call["call_id"] != "c1" || res["call_id"] != "c1" {
		t.Errorf("call_id missing: call=%v result=%v", call["call_id"], res["call_id"])
	}

	if u := st.usage(); u["total"] != int32(230) {
		t.Errorf("turn total = %v, want 230 — done.usage must be the whole turn", u["total"])
	}
}

// A run error becomes a frame and stops the turn, rather than being swallowed.
func TestStreamTurnReportsErrors(t *testing.T) {
	rec := httptest.NewRecorder()
	sse := &sseWriter{w: rec, flusher: rec}

	_, err := streamTurn(sse, seqOf(
		yielded{ev: event(at(0), "model", []*genai.Part{callPart("c1", "get_logs", nil)})},
		yielded{err: errors.New("provider timed out")},
		yielded{ev: event(at(10), "model", []*genai.Part{textPart("never")})},
	))
	if err == nil {
		t.Fatal("streamTurn swallowed the run error")
	}
	frames := parseSSE(t, rec.Body.String())
	if n := count(frames, frameError); n != 1 {
		t.Fatalf("frames = %v, want one error frame", typesOf(frames))
	}
	if n := count(frames, frameText); n != 0 {
		t.Error("events after the error were still projected")
	}
}

// parseSSE decodes the `data: {...}` frames the writer produced.
func parseSSE(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		line, ok := strings.CutPrefix(strings.TrimSpace(block), "data: ")
		if !ok {
			t.Fatalf("frame is not an SSE data line: %q", block)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("frame is not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

// yielded is one item a fake run produces.
type yielded struct {
	ev  *adksession.Event
	err error
}

// seqOf builds a run sequence that honours the range-over-func contract: once
// the loop body returns false, iteration stops. A fake that keeps going past
// that point is not a stricter test, it is an invalid iterator — Go panics on
// it, and the panic hides whatever the test was actually checking.
func seqOf(items ...yielded) iter.Seq2[*adksession.Event, error] {
	return func(yield func(*adksession.Event, error) bool) {
		for _, it := range items {
			if !yield(it.ev, it.err) {
				return
			}
		}
	}
}

// A real round-trip through the session store.
//
// The whole batch rests on one claim: call ids, agent transfers and branches
// survive persistence, because events.content stores the entire genai.Content
// as JSON. That claim is the reason there is no schema migration here, and it
// is exactly the kind of thing a hand-built fixture cannot check — only
// writing through the real store and reading back can. If GORM ever stops
// round-tripping one of these fields, this is the test that says so.
func TestTimelineEndpointSurvivesPersistence(t *testing.T) {
	s := newTestServer(t)
	svc, err := s.engine().NewSessionService()
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	const id = "replay-1"
	if _, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: id,
	}); err != nil {
		t.Fatal(err)
	}

	written := []*adksession.Event{
		event(at(0), "user", []*genai.Part{textPart("看下日志")}),
		event(at(10), "model",
			[]*genai.Part{callPart("t1", transferToAgentTool, map[string]any{"agent_name": "k8s"})},
			transferTo("k8s")),
		event(at(20), "model", []*genai.Part{
			callPart("c1", "get_logs", map[string]any{"ns": "prod"}),
			callPart("c2", "get_logs", map[string]any{"ns": "staging"}),
		}, author("k8s"), branch("root.k8s"), usage(90, 8, 98)),
		event(at(70), "user", []*genai.Part{
			respPart("c2", "get_logs", map[string]any{"lines": 2}),
			respPart("c1", "get_logs", map[string]any{"lines": 1}),
		}, author("k8s"), branch("root.k8s")),
		event(at(90), "model", []*genai.Part{textPart("没有异常。")}, usage(120, 12, 132)),
	}
	sess := sessionOf(t, svc, ctx, id)
	for _, ev := range written {
		if err := svc.AppendEvent(ctx, sess, ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	w := do(t, s, "GET", "/api/sessions/"+id+"/timeline", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Events []map[string]any `json:"events"`
		Usage  map[string]any   `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	// The three fields that had to survive the database.
	calls := only(got.Events, frameToolCall)
	if len(calls) != 2 {
		t.Fatalf("tool calls = %d, want 2: %v", len(calls), typesOf(got.Events))
	}
	for _, c := range calls {
		if c["call_id"] == "" || c["call_id"] == nil {
			t.Errorf("call id lost in persistence: %v", c)
		}
		if c["branch"] != "root.k8s" {
			t.Errorf("branch = %v, want root.k8s", c["branch"])
		}
	}
	if n := count(got.Events, frameAgentTransfer); n != 1 {
		t.Errorf("agent_transfer frames = %d, want 1 — Actions did not survive: %v", n, typesOf(got.Events))
	}

	// And the ids still match up on the way back out, which is the point of
	// keeping them.
	ids := map[any]bool{}
	for _, c := range calls {
		ids[c["call_id"]] = true
	}
	for _, r := range only(got.Events, frameToolResult) {
		if !ids[r["call_id"]] {
			t.Errorf("result id %v matches no call after a round trip", r["call_id"])
		}
	}
	if got.Usage["total"] != float64(230) {
		t.Errorf("usage total = %v, want 230", got.Usage["total"])
	}
}

func sessionOf(t *testing.T, svc adksession.Service, ctx context.Context, id string) adksession.Session {
	t.Helper()
	resp, err := svc.Get(ctx, &adksession.GetRequest{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: id,
	})
	if err != nil || resp.Session == nil {
		t.Fatalf("get session: %v", err)
	}
	return resp.Session
}

// No frame may carry an elapsed time. The one that existed was derived from a
// timestamp ADK reuses across merged parallel responses, so it described
// neither the tool's duration nor the delivery gap — see
// TestParallelToolsGetNoInventedDuration. This guards against it creeping back
// before real per-call timing is wired in.
func TestNoFrameInventsADuration(t *testing.T) {
	frames, _ := run(
		event(at(0), "model", []*genai.Part{callPart("c1", "get_logs", nil)}, usage(1, 1, 2)),
		event(at(100), "user", []*genai.Part{respPart("c1", "get_logs", map[string]any{"n": 1})}),
		event(at(120), "model", []*genai.Part{textPart("ok")}, usage(1, 1, 2)),
	)
	for _, f := range frames {
		for _, k := range []string{"elapsed_ms", "duration_ms", "elapsed", "duration"} {
			if v, present := f[k]; present {
				t.Errorf("%v frame carries %s=%v", f["type"], k, v)
			}
		}
	}
}

// The turn counter is scoped to the invocation, so the same answer gets the
// same number whether it was projected live (one invocation) or on replay
// (every invocation in the session).
func TestTurnCounterResetsPerInvocation(t *testing.T) {
	inv := func(id string) evOpt {
		return func(e *adksession.Event) { e.InvocationID = id }
	}
	frames, _ := run(
		event(at(0), "model", []*genai.Part{textPart("答一")}, inv("i1"), usage(1, 1, 2)),
		event(at(10), "model", []*genai.Part{textPart("答二")}, inv("i2"), usage(1, 1, 2)),
	)
	texts := only(frames, frameText)
	if len(texts) != 2 {
		t.Fatalf("frames = %v, want two text frames", typesOf(frames))
	}
	for i, tx := range texts {
		if tx["turn"] != 1 {
			t.Errorf("text %d turn = %v, want 1 — the counter must reset per invocation", i, tx["turn"])
		}
	}
	if texts[0]["round"] == texts[1]["round"] {
		t.Errorf("both answers share round %v; they are different invocations", texts[0]["round"])
	}
}
