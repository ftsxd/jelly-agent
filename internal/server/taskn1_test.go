package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"
)

// countingSessions counts full session loads.
//
// That is the operation the task list used to do once per session on the
// page, and the one this file exists to keep bounded. Everything else passes
// straight through, so what is measured is the call the projection makes and
// nothing else.
type countingSessions struct {
	adksession.Service
	gets atomic.Int64
}

func (c *countingSessions) Get(ctx context.Context, req *adksession.GetRequest) (*adksession.GetResponse, error) {
	c.gets.Add(1)
	return c.Service.Get(ctx, req)
}

// A second load of the task list must not re-read every session.
//
// Projecting a session means loading every event of it through ADK's service,
// which has no batch API — 20.3ms per session against PostgreSQL, and the
// list walks a page of them. Nothing in a session's projection changes while
// the session does not, so the second page load should read no sessions at
// all.
func TestTheTaskListDoesNotReloadUnchangedSessions(t *testing.T) {
	s := newTestServer(t)
	seedTaskSessions(t, s, 8)
	counter := countSessionLoads(t, s)

	if w := do(t, s, "GET", "/api/tasks?limit=20", ""); w.Code != http.StatusOK {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	first := counter.gets.Load()
	if first == 0 {
		t.Fatal("第一次就没有读任何会话 —— 这个测试没有在测它以为的东西")
	}

	counter.gets.Store(0)
	if w := do(t, s, "GET", "/api/tasks?limit=20", ""); w.Code != http.StatusOK {
		t.Fatalf("second: %d %s", w.Code, w.Body.String())
	}
	if again := counter.gets.Load(); again != 0 {
		t.Errorf("第二次又读了 %d 个会话（第一次 %d 个）—— 每会话全量加载回来了", again, first)
	}
}

// A session that changed must be re-read, or the list shows stale work.
func TestTheTaskListRereadsAChangedSession(t *testing.T) {
	s := newTestServer(t)
	seedTaskSessions(t, s, 1)
	counter := countSessionLoads(t, s)
	do(t, s, "GET", "/api/tasks?limit=20", "")
	counter.gets.Store(0)

	// One more turn in the same session, appended rather than recreated.
	appendTurn(t, s, "n1-0", "inv-2")

	do(t, s, "GET", "/api/tasks?limit=20", "")
	if counter.gets.Load() == 0 {
		t.Error("会话变了却没有重新投影 —— 列表会显示旧的")
	}
}

// The worst path: a filter that matches nothing walks to the scan ceiling.
//
// taskScanMax is 600 sessions. At one full load each that is twelve seconds
// against PostgreSQL, on a request a person is waiting for. It had no test at
// all — the ceiling was a number in a constant nobody exercised.
func TestAFilterThatMatchesNothingDoesNotReloadEverySession(t *testing.T) {
	s := newTestServer(t)
	seedTaskSessions(t, s, 30)

	// Warm, so the assertion is about the second walk and not about the
	// unavoidable first one.
	do(t, s, "GET", "/api/tasks?limit=20&status=blocked", "")

	counter := countSessionLoads(t, s)
	w := do(t, s, "GET", "/api/tasks?limit=20&status=blocked", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if n := counter.gets.Load(); n != 0 {
		t.Errorf("匹配不到时又全量读了 %d 个会话 —— 最坏路径仍然是每会话一次加载", n)
	}
}

// seedTaskSessions writes n sessions that each hold one real task.
func seedTaskSessions(t *testing.T, s *Server, n int) {
	t.Helper()
	for i := range n {
		writeSession(t, s, fmt.Sprintf("n1-%d", i), "inv-1",
			event(at(0), "user", []*genai.Part{textPart("排查一下")}),
			event(at(10), "model", []*genai.Part{callPart("c1", "get_logs", nil)}),
			event(at(20), "user", []*genai.Part{respPart("c1", "get_logs", map[string]any{
				"summary": "命中 3 行", "evidence_id": "e1", "retrievable": true,
			})}),
			event(at(30), "model", []*genai.Part{textPart("看起来是超时")}),
		)
	}
}

// countSessionLoads swaps in a service that counts full session loads.
func countSessionLoads(t *testing.T, s *Server) *countingSessions {
	t.Helper()
	svc, err := s.engine().NewSessionService()
	if err != nil {
		t.Fatal(err)
	}
	c := &countingSessions{Service: svc}
	s.engine().SetSessionServiceForTest(c)
	t.Cleanup(func() { s.engine().SetSessionServiceForTest(svc) })
	return c
}

// appendTurn adds one more round to an existing session.
func appendTurn(t *testing.T, s *Server, id, round string) {
	t.Helper()
	svc, err := s.engine().NewSessionService()
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sess := sessionOf(t, svc, ctx, id)
	for _, ev := range []*adksession.Event{
		event(at(100), "user", []*genai.Part{textPart("再查一次")}),
		event(at(110), "model", []*genai.Part{callPart("c9", "get_logs", nil)}),
		event(at(120), "user", []*genai.Part{respPart("c9", "get_logs", map[string]any{"summary": "ok"})}),
		event(at(130), "model", []*genai.Part{textPart("答案")}),
	} {
		ev.InvocationID = round
		if err := svc.AppendEvent(ctx, sess, ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

// The key has to see a change the clock cannot.
//
// ListPage reports update time in whole seconds, so two events a few hundred
// milliseconds apart share one — and a key made of the session and that
// timestamp would answer the second one from the first one's projection. The
// event count closes it: ADK appends and never rewrites, so a log that has
// changed has either a later second or more events in it.
func TestTheFrameKeySeesAChangeWithinOneSecond(t *testing.T) {
	const (
		id   = "web-1"
		same = int64(1788777287)
	)
	if a, b := frameKey(id, same, 4), frameKey(id, same, 5); a == b {
		t.Errorf("同一秒内多了一条事件，键却没变: %q", a)
	}
	if a, b := frameKey(id, same, 4), frameKey(id, same+1, 4); a == b {
		t.Errorf("时间戳变了，键却没变: %q", a)
	}
	if a, b := frameKey("web-1", same, 4), frameKey("web-2", same, 4); a == b {
		t.Errorf("两个会话共用一个键: %q", a)
	}
	// And the separator is not something a session id can contain, or two
	// different (session, version) pairs could spell the same key.
	if frameKey("a", 1, 23) == frameKey("a", 12, 3) {
		t.Error("键的各段之间没有分隔，不同版本会撞在一起")
	}
}

// A deleted session's projected content must not stay in memory.
//
// Not a correctness property of the list: the scan iterates what ListPage
// returns, and a deleted session is not in it, so the entry would simply
// never be read. It is a property of the purge — purgeSessionTraces exists to
// leave nothing behind, and a cache still holding that conversation's
// projected text is something left behind.
//
// Asserted against the cache rather than the response for exactly that
// reason: the response cannot tell the difference, which is why the
// invalidation had no test until it was checked for one.
func TestDeletingASessionDropsItsCachedProjection(t *testing.T) {
	s := newTestServer(t)
	seedTaskSessions(t, s, 2)

	// Warm the cache through the list.
	if w := do(t, s, "GET", "/api/tasks?limit=20", ""); w.Code != http.StatusOK {
		t.Fatalf("warm: %d %s", w.Code, w.Body.String())
	}
	var before taskListBody
	if err := json.Unmarshal(do(t, s, "GET", "/api/tasks?limit=20", "").Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if len(before.Tasks) < 2 {
		t.Fatalf("precondition: 只有 %d 个任务", len(before.Tasks))
	}

	if w := do(t, s, "DELETE", "/api/sessions/n1-0", ""); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}

	if n := s.frames().held("n1-0"); n != 0 {
		t.Errorf("删除之后缓存里还留着这个会话的 %d 份投影", n)
	}
	// And it is gone from the list, which is the part a person sees.
	var after taskListBody
	if err := json.Unmarshal(do(t, s, "GET", "/api/tasks?limit=20", "").Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	for _, task := range after.Tasks {
		if task.SessionID == "n1-0" {
			t.Errorf("被删掉的会话仍然出现在任务列表里: %+v", task)
		}
	}
	// The surviving session is still answered from the cache, so the
	// invalidation removed one conversation and not the whole cache.
	if n := s.frames().held("n1-1"); n == 0 {
		t.Error("清掉一个会话把别人的投影也清了")
	}
}
