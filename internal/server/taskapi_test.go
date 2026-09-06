package server

import (
	"encoding/json"
	"github.com/jelly-agent/jelly-agent/internal/record"
	"net/http"
	"strings"
	"testing"
	"time"

	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/task"
)

// writeSession stores a run the way the runner would, so the endpoints below
// read real persisted events rather than a hand-made frame list.
//
// The invocation id is stamped here because ADK stamps it on every event it
// writes, and it is the task's identity — a fixture without one would be
// testing a shape production never produces.
func writeSession(t *testing.T, s *Server, id, round string, events ...*adksession.Event) {
	t.Helper()
	svc, err := s.engine().NewSessionService()
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: id,
	}); err != nil {
		t.Fatal(err)
	}
	sess := sessionOf(t, svc, ctx, id)
	for _, ev := range events {
		ev.InvocationID = round
		if err := svc.AppendEvent(ctx, sess, ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

type taskListBody struct {
	Tasks   []Task `json:"tasks"`
	Total   int    `json:"total"`
	HasMore bool   `json:"has_more"`
}

type taskBody struct {
	Task      Task                 `json:"task"`
	Artifacts []Artifact           `json:"artifacts"`
	Results   map[string]ResultRef `json:"results"`
}

// The whole point of the page: a stored run comes back as a task with steps,
// and the answer traces back to the tool results behind it.
func TestTaskEndpointsProjectAStoredRun(t *testing.T) {
	s := newTestServer(t)
	writeSession(t, s, "web-1", "inv-1",
		event(at(0), "user", []*genai.Part{textPart("排查支付服务 CPU 异常")}),
		event(at(10), "model", []*genai.Part{callPart("c1", "get_logs", map[string]any{"svc": "payment-api"})}, usage(90, 8, 98)),
		event(at(40), "user", []*genai.Part{respPart("c1", "get_logs", map[string]any{
			"summary": "命中 12 行", "evidence_id": "e1", "retrievable": true,
		})}),
		event(at(60), "model", []*genai.Part{textPart("根因是连接池耗尽。")}, usage(120, 12, 132)),
	)

	w := do(t, s, "GET", "/api/tasks", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", w.Code, w.Body.String())
	}
	var list taskListBody
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 1 {
		t.Fatalf("tasks = %d: %s", len(list.Tasks), w.Body.String())
	}
	got := list.Tasks[0]
	if got.Title != "排查支付服务 CPU 异常" {
		t.Errorf("title = %q", got.Title)
	}
	if got.SessionID != "web-1" || got.Round == "" {
		t.Errorf("ids = %+v", got)
	}
	// The list carries the shape of the progress but not its innards — a page
	// that shipped every tool payload would be many times the size it draws.
	for _, st := range got.Steps {
		if len(st.Tools) != 0 || st.Note != "" {
			t.Errorf("the list is carrying step detail: %+v", st)
		}
	}

	w = do(t, s, "GET", "/api/tasks/"+got.SessionID+"/"+got.Round, "")
	if w.Code != http.StatusOK {
		t.Fatalf("detail status = %d: %s", w.Code, w.Body.String())
	}
	var detail taskBody
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Task.Steps) != 2 {
		t.Fatalf("steps = %d, want the tool step and the conclusion", len(detail.Task.Steps))
	}
	if detail.Task.Steps[0].Tools[0].EvidenceID != "e1" {
		t.Errorf("step tools = %+v", detail.Task.Steps[0].Tools)
	}
	// 验收 5：the reply is its own thing, quoted verbatim — not an artifact
	// card next to the tool results, and not a generated summary.
	if detail.Task.Reply != "根因是连接池耗尽。" {
		t.Errorf("reply = %q", detail.Task.Reply)
	}
	for _, a := range detail.Artifacts {
		if a.Kind == "report" {
			t.Error("the reply was mixed in with the tool products")
		}
	}
}

// A session with three questions is three tasks — that is the granularity the
// page is built on.
func TestTaskListSplitsASessionByInvocation(t *testing.T) {
	s := newTestServer(t)
	writeSession(t, s, "web-2", "inv-1",
		event(at(0), "user", []*genai.Part{textPart("第一个问题")}),
		event(at(5), "model", []*genai.Part{callPart("c1", "get_pods", nil)}),
		event(at(6), "user", []*genai.Part{respPart("c1", "get_pods", map[string]any{"summary": "ok"})}),
		event(at(10), "model", []*genai.Part{textPart("答案一")}, usage(10, 2, 12)),
	)
	// A second invocation in the same session, written as its own events.
	svc, _ := s.engine().NewSessionService()
	sess := sessionOf(t, svc, t.Context(), "web-2")
	second := event(at(20), "user", []*genai.Part{textPart("第二个问题")})
	second.InvocationID = "inv-2"
	c2 := event(at(24), "model", []*genai.Part{callPart("c2", "get_logs", nil)})
	c2.InvocationID = "inv-2"
	r2 := event(at(25), "user", []*genai.Part{respPart("c2", "get_logs", map[string]any{"summary": "ok"})})
	r2.InvocationID = "inv-2"
	third := event(at(30), "model", []*genai.Part{textPart("答案二")}, usage(10, 2, 12))
	third.InvocationID = "inv-2"
	for _, ev := range []*adksession.Event{second, c2, r2, third} {
		if err := svc.AppendEvent(t.Context(), sess, ev); err != nil {
			t.Fatal(err)
		}
	}

	w := do(t, s, "GET", "/api/tasks", "")
	var list taskListBody
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 2 {
		t.Fatalf("tasks = %d, want one per question: %s", len(list.Tasks), w.Body.String())
	}
	// Newest first, so the page opens on what just happened.
	if list.Tasks[0].StartedAt < list.Tasks[1].StartedAt {
		t.Error("the list is oldest-first")
	}
}

func TestTaskDetailIs404ForAnUnknownRound(t *testing.T) {
	s := newTestServer(t)
	writeSession(t, s, "web-3", "inv-1",
		event(at(0), "user", []*genai.Part{textPart("x")}),
		event(at(5), "model", []*genai.Part{callPart("c1", "get_pods", nil)}),
		event(at(6), "user", []*genai.Part{respPart("c1", "get_pods", map[string]any{"summary": "ok"})}))
	w := do(t, s, "GET", "/api/tasks/web-3/nope", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// An empty deployment shows an empty list, not an error.
func TestTaskListOnAnEmptyDeployment(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "GET", "/api/tasks", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var list taskListBody
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 0 || list.Total != 0 {
		t.Errorf("list = %+v", list)
	}
	// A JSON array, never null — the browser iterates it without a guard.
	if !json.Valid(w.Body.Bytes()) {
		t.Error("invalid JSON")
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	if string(raw["tasks"]) == "null" {
		t.Error(`"tasks" is null; the page iterates it`)
	}
}

// 验收 1，through the endpoint: a run that only talked never reaches the list,
// and the page can say how many were left out rather than losing them.
func TestPlainChatIsNotListedAsATask(t *testing.T) {
	s := newTestServer(t)
	writeSession(t, s, "web-chat", "inv-1",
		event(at(0), "user", []*genai.Part{textPart("你好")}),
		event(at(10), "model", []*genai.Part{textPart("你好！我是运维助手。")}, usage(10, 2, 12)),
	)
	writeSession(t, s, "web-work", "inv-2",
		event(at(0), "user", []*genai.Part{textPart("查一下告警规则")}),
		event(at(5), "model", []*genai.Part{callPart("c1", "list_alert_rules", nil)}),
		event(at(6), "user", []*genai.Part{respPart("c1", "list_alert_rules", map[string]any{"summary": "18 条"})}),
		event(at(10), "model", []*genai.Part{textPart("共 18 条。")}, usage(10, 2, 12)),
	)

	w := do(t, s, "GET", "/api/tasks", "")
	var body struct {
		Tasks       []Task `json:"tasks"`
		SkippedChat int    `json:"skipped_chat"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tasks) != 1 {
		t.Fatalf("tasks = %d: %v", len(body.Tasks), titlesOf(body.Tasks))
	}
	if body.Tasks[0].Title != "查一下告警规则" {
		t.Errorf("title = %q", body.Tasks[0].Title)
	}
	if body.SkippedChat != 1 {
		t.Errorf("skipped = %d; the page cannot say why a chat is missing", body.SkippedChat)
	}
}

// 验收 8：a follow-up sent with a task id joins that task instead of opening a
// second one that tells half the story.
func TestALinkedFollowUpJoinsTheOriginalTask(t *testing.T) {
	s := newTestServer(t)
	writeSession(t, s, "web-link", "inv-1",
		event(at(0), "user", []*genai.Part{textPart("巡检磁盘")}),
		event(at(5), "model", []*genai.Part{callPart("c1", "check_disk", nil)}),
		event(at(6), "user", []*genai.Part{respPart("c1", "check_disk", map[string]any{"summary": "缺少阈值"})}),
		event(at(10), "model", []*genai.Part{textPart("需要一个容量阈值。")}, usage(10, 2, 12)),
	)
	svc, _ := s.engine().NewSessionService()
	sess := sessionOf(t, svc, t.Context(), "web-link")
	for _, ev := range []*adksession.Event{
		event(at(20), "user", []*genai.Part{textPart("用 85%")}),
		event(at(24), "model", []*genai.Part{callPart("c2", "check_disk", nil)}),
		event(at(25), "user", []*genai.Part{respPart("c2", "check_disk", map[string]any{"summary": "3 台超阈值"})}),
		event(at(30), "model", []*genai.Part{textPart("3 台机器超过 85%。")}, usage(10, 2, 12)),
	} {
		ev.InvocationID = "inv-2"
		if err := svc.AppendEvent(t.Context(), sess, ev); err != nil {
			t.Fatal(err)
		}
	}

	// Without the link the two runs are two tasks — the state before the user
	// says "this is the same job".
	w := do(t, s, "GET", "/api/tasks", "")
	var before struct {
		Tasks []Task `json:"tasks"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &before)
	if len(before.Tasks) != 2 {
		t.Fatalf("before linking: tasks = %d", len(before.Tasks))
	}

	if err := task.Link(s.engine().SessionDBPath(), "web-link/inv-1", "web-link", "inv-2"); err != nil {
		t.Fatal(err)
	}

	w = do(t, s, "GET", "/api/tasks", "")
	var after struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Tasks) != 1 {
		t.Fatalf("after linking: tasks = %d: %v", len(after.Tasks), titlesOf(after.Tasks))
	}
	got := after.Tasks[0]
	if got.Title != "巡检磁盘" {
		t.Errorf("title = %q; the follow-up retitled the task", got.Title)
	}
	if len(got.Runs) != 2 {
		t.Errorf("runs = %v", got.Runs)
	}
}

func titlesOf(ts []Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Title)
	}
	return out
}

// 验收 10：an ordinary tool call does not become a product of the run.
//
// Every gatewayed call is stored, so "was stored" cannot be the test — it
// would put an empty listing next to a four-megabyte log and call them both
// products. What earns a place is being worth opening on its own. The small
// ones are still reachable, from the step that produced them, which is what
// `results` is for.
func TestSmallResultsStayInTheirStepInsteadOfBecomingProducts(t *testing.T) {
	s := newTestServer(t)
	store, err := s.engine().Records()
	if err != nil {
		t.Fatal(err)
	}
	sc := record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-art"}
	small, err := store.Put(t.Context(), record.Record{
		Scope: sc, InvocationID: "inv-1", CallID: "c1", Tool: "list_targets",
		At: time.Now(), Payload: []byte(`{"list":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	big, err := store.Put(t.Context(), record.Record{
		Scope: sc, InvocationID: "inv-1", CallID: "c2", Tool: "query_range",
		At: time.Now(), Payload: []byte(`{"data":"` + strings.Repeat("x", 40000) + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	writeSession(t, s, "web-art", "inv-1",
		event(at(0), "user", []*genai.Part{textPart("查一下")}),
		event(at(5), "model", []*genai.Part{
			callPart("c1", "list_targets", nil), callPart("c2", "query_range", nil),
		}),
		event(at(6), "user", []*genai.Part{
			respPart("c1", "list_targets", map[string]any{"summary": "空", "evidence_id": small, "retrievable": true}),
			respPart("c2", "query_range", map[string]any{"summary": "两条曲线", "evidence_id": big, "retrievable": true}),
		}),
		event(at(10), "model", []*genai.Part{textPart("好了。")}, usage(10, 2, 12)),
	)

	w := do(t, s, "GET", "/api/tasks/web-art/inv-1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got taskBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].Label != big {
		t.Fatalf("artifacts = %+v; only the substantial one belongs on the shelf", got.Artifacts)
	}
	// Both are still reachable from their step — the small one is not lost,
	// it is just not a product of the run.
	if len(got.Results) != 2 {
		t.Errorf("results = %d, want both calls addressable from the step", len(got.Results))
	}
	if r, ok := got.Results["c1"]; !ok || !r.Retrievable || r.Label != small {
		t.Errorf("the small result is not reachable from its step: %+v", got.Results["c1"])
	}
}

// 验收 4/7：retrievable is what the store can answer now, not what the model
// was told then. Historical events carry no such flag at all, and deriving it
// from them marked every older result unsaved while its bytes sat readable in
// the database.
func TestRetrievabilityComesFromTheStoreNotTheOldEvent(t *testing.T) {
	s := newTestServer(t)
	store, err := s.engine().Records()
	if err != nil {
		t.Fatal(err)
	}
	label, err := store.Put(t.Context(), record.Record{
		Scope:        record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-old"},
		InvocationID: "inv-1", CallID: "c1", Tool: "list_alert_rules",
		At: time.Now(), Payload: []byte(`{"data":"` + strings.Repeat("y", 40000) + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The response has the shape events had before "retrievable" existed.
	writeSession(t, s, "web-old", "inv-1",
		event(at(0), "user", []*genai.Part{textPart("查告警")}),
		event(at(5), "model", []*genai.Part{callPart("c1", "list_alert_rules", nil)}),
		event(at(6), "user", []*genai.Part{respPart("c1", "list_alert_rules", map[string]any{
			"summary": "18 条", "evidence_id": label, "truncated": true,
		})}),
		event(at(10), "model", []*genai.Part{textPart("共 18 条。")}, usage(10, 2, 12)),
	)

	w := do(t, s, "GET", "/api/tasks/web-old/inv-1", "")
	var got taskBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v", got.Artifacts)
	}
	if !got.Artifacts[0].Retrievable {
		t.Error("a stored result was reported unreadable because its event predates the flag")
	}
	if got.Artifacts[0].Complete {
		t.Error("a truncated result was reported complete")
	}
}
