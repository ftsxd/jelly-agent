package server

import (
	"context"
	"encoding/json"
	"github.com/jelly-agent/jelly-agent/internal/record"
	"net/http"
	"strconv"
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
	Tasks      []Task `json:"tasks"`
	Total      int    `json:"total"`
	TotalExact bool   `json:"total_exact"`
	HasMore    bool   `json:"has_more"`
	Scanned    int    `json:"scanned_sessions"`
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
	if r, ok := got.Results["inv-1/c1"]; !ok || !r.Retrievable || r.Label != small {
		t.Errorf("the small result is not reachable from its step: %+v", got.Results["inv-1/c1"])
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

// A folded task holds more than one run, and every run numbers its calls from
// the start — so both runs' first call is "c1".
//
// The store's key says as much: (app, user, session, invocation, call). The
// detail used to index results and durations by call id alone, which put the
// first run's bytes and the first run's elapsed time under the follow-up's
// step. Nothing about that looks wrong on screen, which is what makes it worth
// a test: it is a correct-looking answer to the wrong question.
func TestAFoldedTaskKeepsEachRunsCallsApart(t *testing.T) {
	s := newTestServer(t)
	store, err := s.engine().Records()
	if err != nil {
		t.Fatal(err)
	}
	sc := record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-dup"}
	refs := map[string]string{}
	for _, run := range []struct{ inv, body string }{
		{"inv-1", "第一轮"}, {"inv-2", "第二轮"},
	} {
		ref, err := store.Put(t.Context(), record.Record{
			Scope: sc, InvocationID: run.inv, CallID: "c1",
			Tool: "get_logs", At: time.Now(), Payload: []byte(run.body),
		})
		if err != nil {
			t.Fatal(err)
		}
		refs[run.inv] = ref
	}

	writeSession(t, s, "web-dup", "inv-1",
		event(at(0), "user", []*genai.Part{textPart("看看日志")}),
		event(at(5), "model", []*genai.Part{callPart("c1", "get_logs", nil)}),
		event(at(6), "user", []*genai.Part{respPart("c1", "get_logs", map[string]any{
			"summary": "第一轮", "evidence_id": refs["inv-1"], "retrievable": true})}),
		event(at(10), "model", []*genai.Part{textPart("有超时。")}, usage(10, 2, 12)),
	)
	svc, _ := s.engine().NewSessionService()
	sess := sessionOf(t, svc, t.Context(), "web-dup")
	for _, ev := range []*adksession.Event{
		event(at(20), "user", []*genai.Part{textPart("再看看")}),
		event(at(24), "model", []*genai.Part{callPart("c1", "get_logs", nil)}),
		event(at(25), "user", []*genai.Part{respPart("c1", "get_logs", map[string]any{
			"summary": "第二轮", "evidence_id": refs["inv-2"], "retrievable": true})}),
		event(at(30), "model", []*genai.Part{textPart("还在超时。")}, usage(10, 2, 12)),
	} {
		ev.InvocationID = "inv-2"
		if err := svc.AppendEvent(t.Context(), sess, ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := task.Link(s.engine().SessionDBPath(), "web-dup/inv-1", "web-dup", "inv-2"); err != nil {
		t.Fatal(err)
	}

	w := do(t, s, "GET", "/api/tasks/web-dup/inv-1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got taskBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	// Both rows are in the index, under keys that name their run.
	for _, inv := range []string{"inv-1", "inv-2"} {
		r, ok := got.Results[inv+"/c1"]
		if !ok {
			t.Fatalf("results = %+v; %s/c1 is missing", got.Results, inv)
		}
		if r.Label != refs[inv] {
			t.Errorf("%s/c1 指向 %s，应当是 %s", inv, r.Label, refs[inv])
		}
	}

	// And each step's call carries the run it belongs to, which is what the
	// console needs to look it up.
	seen := map[string]string{}
	for _, st := range got.Task.Steps {
		for _, tl := range st.Tools {
			if tl.CallID != "c1" {
				continue
			}
			if tl.Round == "" {
				t.Fatalf("step %s 的调用没有带 round: %+v", st.ID, tl)
			}
			seen[tl.Round] = tl.Summary
		}
	}
	if seen["inv-1"] != "第一轮" || seen["inv-2"] != "第二轮" {
		t.Errorf("每轮的结果没有落回自己的调用: %+v", seen)
	}
}

// The list reads back as far as it has to, instead of folding a fixed prefix
// of the sessions and paging inside that.
//
// The prefix was the bug: a filter matching nothing in the recent sessions
// answered "no tasks" while matching ones sat one session further back, and
// page two of anything was almost always empty, because a prefix rarely held
// two pages' worth. Both look like "there is nothing there", which is why this
// is worth a test rather than an eyeball.
func TestTheListReadsPastTheFirstPageOfSessions(t *testing.T) {
	s := newTestServer(t)
	const n = taskScanPage + 15 // more sessions than one scan page
	for i := range n {
		id := "web-" + strconv.Itoa(i)
		writeSession(t, s, id, "inv-1",
			event(at(i*100), "user", []*genai.Part{textPart("查一下 " + id)}),
			event(at(i*100+5), "model", []*genai.Part{callPart("c1", "get_logs", nil)}),
			event(at(i*100+6), "user", []*genai.Part{
				respPart("c1", "get_logs", map[string]any{"summary": "3 行"})}),
			event(at(i*100+9), "model", []*genai.Part{textPart("看完了。")}, usage(10, 2, 12)),
		)
	}

	// Every task is reachable, not just the ones in the first scan page.
	w := do(t, s, "GET", "/api/tasks?limit=200", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var all taskListBody
	if err := json.Unmarshal(w.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Tasks) != n {
		t.Errorf("tasks = %d, want %d — 扫描在会话第一页就停了", len(all.Tasks), n)
	}
	if !all.TotalExact {
		t.Errorf("扫完了全部会话却把总数报成了下限: scanned=%d", all.Scanned)
	}

	// And a second page holds what the first one left, rather than nothing.
	w = do(t, s, "GET", "/api/tasks?limit=10&offset=40", "")
	var page taskListBody
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 10 {
		t.Fatalf("第二页 = %d 条，应当是 10 条", len(page.Tasks))
	}
	if page.Tasks[0].ID != all.Tasks[40].ID {
		t.Errorf("分页错位: 第 40 条是 %s，整表里是 %s", page.Tasks[0].ID, all.Tasks[40].ID)
	}
}

// Having enough tasks is not the same as having the right ones.
//
// Sessions are listed by when they were last touched; tasks are ordered by
// when they started. The two come apart in the ordinary case: a session stays
// near the top of the list because somebody chatted in it this afternoon,
// while every task it contains is from last month. A scan that stopped as soon
// as it had filled a page therefore put last month's work on top, with
// yesterday's sitting unread in the very next session.
//
// Every other test here has one task per session, which is exactly the shape
// that cannot show this.
func TestTheNewestTaskWinsEvenFromAnUnscannedSession(t *testing.T) {
	s := newTestServer(t)

	// Touched least recently of all, so it lands on the second page of
	// sessions — while holding the newest task of them all.
	writeSession(t, s, "web-newest", "inv-1",
		event(at(300_000_000), "user", []*genai.Part{textPart("最新的那件事")}),
		event(at(300_000_005), "model", []*genai.Part{callPart("c1", "get_logs", nil)}),
		event(at(300_000_006), "user", []*genai.Part{
			respPart("c1", "get_logs", map[string]any{"summary": "3 行"})}),
		event(at(300_000_009), "model", []*genai.Part{textPart("看完了。")}, usage(10, 2, 12)),
	)
	// A full page of sessions whose work is old but whose last message is
	// recent — the shape that keeps a session at the top of the list without
	// putting anything new in it.
	for i := range taskScanPage {
		id := "web-busy-" + strconv.Itoa(i)
		writeSession(t, s, id, "inv-1",
			event(at(1000+i*10), "user", []*genai.Part{textPart("很久以前的活 " + id)}),
			event(at(1005+i*10), "model", []*genai.Part{callPart("c1", "get_logs", nil)}),
			event(at(1006+i*10), "user", []*genai.Part{
				respPart("c1", "get_logs", map[string]any{"summary": "3 行"})}),
			event(at(1009+i*10), "model", []*genai.Part{textPart("看完了。")}, usage(10, 2, 12)),
		)
		// Today's chat in the same session: no tool ran, so it is not a task —
		// but it is what the session's last-updated time now says.
		appendRound(t, s, id, "inv-2",
			event(at(400_000_000+i*10), "user", []*genai.Part{textPart("辛苦了")}),
			event(at(400_000_002+i*10), "model", []*genai.Part{textPart("不客气。")}, usage(2, 1, 3)),
		)
	}

	w := do(t, s, "GET", "/api/tasks?limit=1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var page taskListBody
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(page.Tasks))
	}
	if page.Tasks[0].SessionID != "web-newest" {
		t.Errorf("第一条是 %s 的任务；最新的那件事在下一页会话里，扫描提前停了",
			page.Tasks[0].SessionID)
	}
}

// appendRound adds another run to a session that already exists.
func appendRound(t *testing.T, s *Server, id, round string, events ...*adksession.Event) {
	t.Helper()
	svc, err := s.engine().NewSessionService()
	if err != nil {
		t.Fatal(err)
	}
	sess := sessionOf(t, svc, t.Context(), id)
	for _, ev := range events {
		ev.InvocationID = round
		if err := svc.AppendEvent(t.Context(), sess, ev); err != nil {
			t.Fatal(err)
		}
	}
}

// The frontier has to be an upper bound, and a truncated one is not.
//
// Session update times come back as whole seconds; a task's start is in
// milliseconds. Multiplying the truncated value put the frontier up to 999ms
// below where it belongs — under tasks that genuinely sit above it — so the
// scan could stop one session early and miss a newer task by a fraction of a
// second. Everything in this fixture happens inside one second, which is the
// only place the difference is visible.
func TestTheScanDoesNotStopOnATruncatedFrontier(t *testing.T) {
	s := newTestServer(t)
	const sec = 1_000_000 // a whole number of seconds past the test's base time

	// Least recently touched, so it lands on the second page of sessions —
	// holding a task newer than anything on the first page.
	writeSession(t, s, "web-newest", "inv-1",
		event(at(sec+500), "user", []*genai.Part{textPart("同一秒里更晚的那件事")}),
		event(at(sec+501), "model", []*genai.Part{callPart("c1", "get_logs", nil)}),
		event(at(sec+502), "user", []*genai.Part{
			respPart("c1", "get_logs", map[string]any{"summary": "3 行"})}),
		event(at(sec+503), "model", []*genai.Part{textPart("看完了。")}, usage(10, 2, 12)),
	)
	// A full page of sessions whose work is earlier in the same second, kept
	// at the top of the list by a chat that is later in that same second.
	for i := range taskScanPage {
		id := "web-busy-" + strconv.Itoa(i)
		writeSession(t, s, id, "inv-1",
			event(at(sec+100+i), "user", []*genai.Part{textPart("更早的活 " + id)}),
			event(at(sec+101+i), "model", []*genai.Part{callPart("c1", "get_logs", nil)}),
			event(at(sec+102+i), "user", []*genai.Part{
				respPart("c1", "get_logs", map[string]any{"summary": "3 行"})}),
			event(at(sec+103+i), "model", []*genai.Part{textPart("看完了。")}, usage(10, 2, 12)),
		)
		appendRound(t, s, id, "inv-2",
			event(at(sec+900+i), "user", []*genai.Part{textPart("辛苦了")}),
			event(at(sec+901+i), "model", []*genai.Part{textPart("不客气。")}, usage(2, 1, 3)),
		)
	}

	w := do(t, s, "GET", "/api/tasks?limit=1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var page taskListBody
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(page.Tasks))
	}
	if page.Tasks[0].SessionID != "web-newest" {
		t.Errorf("第一条是 %s 的任务；边界被截断到整秒，扫描早停了不到一秒",
			page.Tasks[0].SessionID)
	}
}

// The records probe is answered for a whole page of sessions at once.
//
// It is the only expensive test in the admission rule and the scan reads back
// hundreds of sessions. Making it lazy helped the common case and left the
// worst one alone: a deployment whose sessions are mostly plain conversation
// is exactly where every session needs the probe, and that is 600 queries.
//
// Counted through the store, because "one per page" is a property of the call
// site and a test of the query alone stays green when the call site loops.
func TestTheRecordsProbeIsBatchedPerPage(t *testing.T) {
	s := newTestServer(t)
	// Sessions that look like conversation by every other measure, so the
	// probe is the only thing that can settle them — the worst case.
	for i := range 5 {
		writeSession(t, s, "web-chat-"+strconv.Itoa(i), "inv-1",
			event(at(1000+i*10), "user", []*genai.Part{textPart("你好")}),
			event(at(1005+i*10), "model", []*genai.Part{textPart("你好，有什么可以帮你的？")}, usage(2, 1, 3)),
		)
	}

	counted := &countingRecords{Store: mustRecords(t, s)}
	s.recordsProbe = counted.RecordedRuns

	if w := do(t, s, "GET", "/api/tasks", ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if counted.calls != 1 {
		t.Errorf("查了 %d 次存储；一页会话只该查 1 次", counted.calls)
	}
	if counted.sessions != 5 {
		t.Errorf("这一次查询覆盖了 %d 个会话，应当是 5 个", counted.sessions)
	}
}

// IsTask asks the expensive question last, and only when nothing cheaper has
// settled it.
func TestTheRecordsProbeIsOnlyAskedWhenItCanChangeTheAnswer(t *testing.T) {
	asked := 0
	probe := func() bool { asked++; return false }

	// A run that called a tool is work by that fact alone.
	work := Task{SessionID: "web-1", Status: TaskCompleted, Chat: false}
	if !IsTask(work, probe) {
		t.Error("调用过工具的运行没有被当成任务")
	}
	// A failed run is something a person has to act on, whatever it stored.
	failed := Task{SessionID: "web-1", Status: TaskFailed, Chat: true}
	if !IsTask(failed, probe) {
		t.Error("失败的运行没有被当成任务")
	}
	// A scheduled run is work by definition.
	sched := Task{SessionID: schedulePrefix + "nightly", Status: TaskCompleted, Chat: true}
	if !IsTask(sched, probe) {
		t.Error("定时任务没有被当成任务")
	}
	if asked != 0 {
		t.Errorf("已经能判定的情况下还查了 %d 次存储", asked)
	}

	// Only the genuinely ambiguous case pays for the query.
	chat := Task{SessionID: "web-1", Status: TaskCompleted, Chat: true}
	if IsTask(chat, probe) {
		t.Error("没有工具、没有产物、没有失败的对话被当成了任务")
	}
	if asked != 1 {
		t.Errorf("存储被查了 %d 次，只该查 1 次", asked)
	}
	if !IsTask(chat, someRecords) {
		t.Error("留下了产物的运行没有被当成任务")
	}
}

// countingRecords counts how often the batched probe is called, and how wide
// each call was.
type countingRecords struct {
	*record.Store
	calls    int
	sessions int
}

func (c *countingRecords) RecordedRuns(ctx context.Context, app, user string, ids []string) (map[string]map[string]bool, error) {
	c.calls++
	c.sessions += len(ids)
	return c.Store.RecordedRuns(ctx, app, user, ids)
}

func mustRecords(t *testing.T, s *Server) *record.Store {
	t.Helper()
	store, err := s.engine().Records()
	if err != nil {
		t.Fatal(err)
	}
	return store
}
