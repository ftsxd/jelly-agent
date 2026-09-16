package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

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
	end := r.start("web-1", "inv-2", "web-1/inv-1", nil)

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
	end := r.start("web-1", "inv-1", "", nil)
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
	s.runs().start("web-live", "inv-2", "web-live/inv-1", nil)

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
	endA := r.start("web-1", "inv-2", "web-1/inv-1", nil)
	endB := r.start("web-1", "inv-3", "web-1/inv-1", nil)

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
	endA := r.start("web-1", "inv-2", "web-1/inv-1", nil)
	endB := r.start("web-1", "inv-3", "web-1/inv-1", nil)

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

// The chat view asks by session, and a run joined to an earlier task is filed
// under that task's id — which is not the id the asker has.
//
// Answering per-task only, the conversation showing a continuation had no way
// to learn that anything was in flight: it knows the session it is displaying
// and nothing else.
func TestSessionStatusFindsARunFiledUnderAnotherTaskOfTheSameSession(t *testing.T) {
	r := newRunRegistry()
	end := r.start("web-1", "inv-2", "web-1/inv-1", nil)

	if st, ok := r.sessionStatus("web-1"); !ok || st != TaskRunning {
		t.Fatalf("session status = %q/%v, want running", st, ok)
	}
	if _, ok := r.sessionStatus("web-2"); ok {
		t.Error("另一条会话也被报成了有运行在跑")
	}
	end(TaskCompleted)
	if st, ok := r.sessionStatus("web-1"); !ok || st != TaskCompleted {
		t.Errorf("after finishing = %q/%v, want completed", st, ok)
	}
}

// A session nobody is running says nothing, rather than saying it is idle.
//
// The distinction is the whole point of the flag: after a restart, or past the
// outcome TTL, this process genuinely does not know how a turn ended, and a
// page told "not running" would render a half-written answer as the finished
// one — the failure this was added to stop.
func TestSessionStatusIsSilentWhenNothingIsKnown(t *testing.T) {
	r := newRunRegistry()
	if st, ok := r.sessionStatus("web-1"); ok {
		t.Errorf("status = %q/%v, want nothing known", st, ok)
	}
	if _, ok := r.sessionStatus(""); ok {
		t.Error("空会话 id 也拿到了状态")
	}
}

// And the replay endpoint carries it: leaving the chat view and coming back is
// a fresh replay, so that is the only place the page can find out that the run
// it walked away from is still going.
func TestTheTimelineSaysWhetherTheSessionIsStillRunning(t *testing.T) {
	s := newTestServer(t)
	writeSession(t, s, "web-live", "inv-1",
		event(at(0), "user", []*genai.Part{textPart("巡检一下 k8s-test 集群")}),
		event(at(5), "model", []*genai.Part{callPart("c1", "query_instant", nil)}),
	)

	timelineStatus := func() (string, bool) {
		t.Helper()
		w := do(t, s, "GET", "/api/sessions/web-live/timeline", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		st, ok := got["status"].(string)
		return st, ok
	}

	if st, ok := timelineStatus(); ok {
		t.Errorf("replay of an untracked session = %q, want no status at all", st)
	}
	end := s.runs().start("web-live", "inv-1", "", nil)
	if st, ok := timelineStatus(); !ok || st != TaskRunning {
		t.Errorf("replay while running = %q/%v; 页面看不出这轮还在跑", st, ok)
	}
	end(TaskCompleted)
	if st, ok := timelineStatus(); !ok || st != TaskCompleted {
		t.Errorf("replay just after the turn = %q/%v, want completed", st, ok)
	}
}

// Someone who reopens a conversation mid-turn must be able to end it. The run
// is driven by the request that started it, so the only thing that can stop it
// from another request is the cancel the registry is holding.
func TestStopSessionCancelsRunsStartedByAnotherRequest(t *testing.T) {
	s := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	end := s.runs().start("web-live", "inv-1", "", cancel)

	// A conversation with nothing running is not an error — the caller wanted
	// nothing running, and nothing is.
	if w := do(t, s, "POST", "/api/sessions/web-idle/stop", `{}`); w.Code != 200 {
		t.Fatalf("空闲会话被当成错误：%d %s", w.Code, w.Body.String())
	}
	if ctx.Err() != nil {
		t.Fatal("停止另一个会话却取消了这一个")
	}

	w := do(t, s, "POST", "/api/sessions/web-live/stop", `{}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"stopped":1`) {
		t.Fatalf("停止没有生效：%d %s", w.Code, w.Body.String())
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("运行中的那一轮没有被取消")
	}
	// The handler still reports its own outcome; stopping does not retire the
	// run behind its back.
	if st, ok := s.runs().sessionStatus("web-live"); !ok || st != TaskRunning {
		t.Fatalf("取消后立刻就不算运行中了：%q %v", st, ok)
	}
	end(TaskCancelled)
	if st, _ := s.runs().sessionStatus("web-live"); st != TaskCancelled {
		t.Fatalf("结束后的状态是 %q", st)
	}
}
