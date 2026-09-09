package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"google.golang.org/genai"

	adksession "google.golang.org/adk/session"

	"github.com/jelly-agent/jelly-agent/internal/task"
)

// A run joined to an earlier task is displayed under that task's id, so its
// status has to be filed there too.
//
// Filed under its own invocation, it landed somewhere nothing looks: the task
// the user was watching showed idle for the whole of the continuation, and
// then took on whatever the first run had ended as. Both are wrong in the
// direction that looks plausible.
func TestAContinuationsStatusIsFiledUnderTheTaskItJoined(t *testing.T) {
	r := newRunRegistry()
	end := r.start("web-1", "inv-2", "web-1/inv-1")

	if st, ok := r.status("web-1/inv-1"); !ok || st != TaskRunning {
		t.Fatalf("folded task status = %q/%v, want running", st, ok)
	}
	if _, ok := r.status("web-1/inv-2"); ok {
		t.Error("状态也登记在了运行自己的 id 下，两处会互相矛盾")
	}
	end(TaskCompleted)
	if st, _ := r.status("web-1/inv-1"); st != TaskCompleted {
		t.Errorf("after finishing = %q, want completed", st)
	}
}

// With no task to join, a run is its own task and is filed under itself.
func TestAnUnjoinedRunIsFiledUnderItself(t *testing.T) {
	r := newRunRegistry()
	end := r.start("web-1", "inv-1", "")
	if st, ok := r.status("web-1/inv-1"); !ok || st != TaskRunning {
		t.Fatalf("status = %q/%v", st, ok)
	}
	end(TaskFailed)
	if st, _ := r.status("web-1/inv-1"); st != TaskFailed {
		t.Errorf("status = %q, want failed", st)
	}
}

// And the list reads it there: a running continuation makes the folded task
// running, not the standalone run that no longer exists as a task.
func TestTheListShowsAJoinedRunAsTheTasksStatus(t *testing.T) {
	s := newTestServer(t)
	writeSession(t, s, "web-live", "inv-1",
		event(at(0), "user", []*genai.Part{textPart("巡检磁盘")}),
		event(at(5), "model", []*genai.Part{callPart("c1", "check_disk", nil)}),
		event(at(6), "user", []*genai.Part{respPart("c1", "check_disk", map[string]any{"summary": "缺少阈值"})}),
		event(at(10), "model", []*genai.Part{textPart("需要一个阈值。")}, usage(10, 2, 12)),
	)
	svc, _ := s.engine().NewSessionService()
	sess := sessionOf(t, svc, t.Context(), "web-live")
	for _, ev := range []*adksession.Event{
		event(at(20), "user", []*genai.Part{textPart("用 85%")}),
		event(at(24), "model", []*genai.Part{callPart("c2", "check_disk", nil)}),
	} {
		ev.InvocationID = "inv-2"
		if err := svc.AppendEvent(t.Context(), sess, ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := task.Link(stateDBOf(t, s), "web-live/inv-1", "web-live", "inv-2"); err != nil {
		t.Fatal(err)
	}
	s.runs().start("web-live", "inv-2", "web-live/inv-1")

	w := do(t, s, "GET", "/api/tasks", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("tasks = %d: %v", len(got.Tasks), titlesOf(got.Tasks))
	}
	if got.Tasks[0].Status != TaskRunning {
		t.Errorf("status = %q; 续跑中的任务显示成了空闲", got.Tasks[0].Status)
	}
}

// A task can have more than one run in flight: two browsers continuing the
// same task, or a scheduled trigger landing while somebody follows up by hand.
//
// With one entry per task the second registration overwrote the first, and
// whichever finished first deleted the only record — so the task showed
// completed while the other run was still working. That is the worst shape for
// this particular lie: it looks exactly like a finished answer.
func TestATaskWithTwoRunsInFlightStaysRunningUntilBothEnd(t *testing.T) {
	r := newRunRegistry()
	endA := r.start("web-1", "inv-2", "web-1/inv-1")
	endB := r.start("web-1", "inv-3", "web-1/inv-1")

	if st, ok := r.status("web-1/inv-1"); !ok || st != TaskRunning {
		t.Fatalf("status = %q/%v, want running", st, ok)
	}
	endA(TaskCompleted)
	if st, _ := r.status("web-1/inv-1"); st != TaskRunning {
		t.Errorf("一个运行结束后任务就报 %q，另一个还在跑", st)
	}
	endB(TaskCompleted)
	if st, ok := r.status("web-1/inv-1"); !ok || st != TaskCompleted {
		t.Errorf("status = %q/%v, want completed", st, ok)
	}
}

// The finish function is called twice on purpose — the handler calls it, and a
// deferred safety net calls it again on any path that skipped the first. The
// second call must retire nothing: the run it names has already gone, and the
// entry it finds now belongs to somebody else.
func TestFinishingTwiceRetiresOneRun(t *testing.T) {
	r := newRunRegistry()
	endA := r.start("web-1", "inv-2", "web-1/inv-1")
	endB := r.start("web-1", "inv-3", "web-1/inv-1")

	endA(TaskCompleted)
	endA(TaskCancelled)
	if st, _ := r.status("web-1/inv-1"); st != TaskRunning {
		t.Errorf("重复调用结束函数把另一个运行也注销了: status = %q", st)
	}

	endB(TaskCompleted)
	if st, _ := r.status("web-1/inv-1"); st != TaskCompleted {
		t.Errorf("status = %q, want completed", st)
	}
}
