package server

import (
	"encoding/json"
	"net/http"
	"testing"

	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/engine"
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
	Task      Task       `json:"task"`
	Artifacts []Artifact `json:"artifacts"`
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
		if len(st.Tools) != 0 || st.Progress != "" {
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
	// The answer is an artifact of the run, because it is what the run was for.
	var report *Artifact
	for i := range detail.Artifacts {
		if detail.Artifacts[i].Kind == "report" {
			report = &detail.Artifacts[i]
		}
	}
	if report == nil || report.Text != "根因是连接池耗尽。" {
		t.Errorf("artifacts = %+v", detail.Artifacts)
	}
}

// A session with three questions is three tasks — that is the granularity the
// page is built on.
func TestTaskListSplitsASessionByInvocation(t *testing.T) {
	s := newTestServer(t)
	writeSession(t, s, "web-2", "inv-1",
		event(at(0), "user", []*genai.Part{textPart("第一个问题")}),
		event(at(10), "model", []*genai.Part{textPart("答案一")}, usage(10, 2, 12)),
	)
	// A second invocation in the same session, written as its own events.
	svc, _ := s.engine().NewSessionService()
	sess := sessionOf(t, svc, t.Context(), "web-2")
	second := event(at(20), "user", []*genai.Part{textPart("第二个问题")})
	second.InvocationID = "inv-2"
	third := event(at(30), "model", []*genai.Part{textPart("答案二")}, usage(10, 2, 12))
	third.InvocationID = "inv-2"
	for _, ev := range []*adksession.Event{second, third} {
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
	writeSession(t, s, "web-3", "inv-1", event(at(0), "user", []*genai.Part{textPart("x")}))
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
