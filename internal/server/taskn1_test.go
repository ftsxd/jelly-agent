package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

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

// The very first request, on an empty cache, must not read sessions one at a
// time.
//
// This is the property, and the earlier version of this test did not assert
// it: it required the first request to load every session and only checked
// that the second did not. That made a cache look like a fix. A new process,
// an evicted entry, or a page of sessions that all just changed would still
// have paid one full Get per session — 20.3ms each against PostgreSQL, and
// the scan ceiling is 600 of them.
//
// No warm-up anywhere in this file, deliberately.
func TestTheTaskListNeverReadsSessionsOneAtATime(t *testing.T) {
	for _, n := range []int{4, 16, 40} {
		t.Run(fmt.Sprintf("%d个会话", n), func(t *testing.T) {
			s := newTestServer(t)
			seedTaskSessions(t, s, n)
			counter := countSessionLoads(t, s)

			if w := do(t, s, "GET", "/api/tasks?limit=20", ""); w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			// Constant, and the constant is zero: the list reads a page of
			// events in one query and never opens the session service.
			if got := counter.gets.Load(); got != 0 {
				t.Errorf("冷缓存下读了 %d 次完整会话，%d 个会话 —— 逐会话加载还在", got, n)
			}
		})
	}
}

// A page still has to show what changed, now that it is not read per session.
func TestTheTaskListShowsAChangedSession(t *testing.T) {
	s := newTestServer(t)
	seedTaskSessions(t, s, 3)

	var before taskListBody
	if err := json.Unmarshal(do(t, s, "GET", "/api/tasks?limit=20", "").Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	appendTurn(t, s, "n1-0", "inv-2")

	var after taskListBody
	if err := json.Unmarshal(do(t, s, "GET", "/api/tasks?limit=20", "").Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Tasks) <= len(before.Tasks) {
		t.Errorf("新一轮没有出现在列表里: 之前 %d 个任务，之后 %d 个",
			len(before.Tasks), len(after.Tasks))
	}
}

// The worst path: a filter that matches nothing walks to the scan ceiling.
//
// taskScanMax is 600 sessions. At one full load each that is twelve seconds
// against PostgreSQL, on a request a person is waiting for. It had no test at
// all — the ceiling was a number in a constant nobody exercised.
// The worst path, cold: a filter that matches nothing walks to the scan
// ceiling on the very first request.
//
// taskScanMax is 600 sessions. At one full load each that was twelve seconds
// against PostgreSQL, on a request a person is waiting for — and no warm-up
// helps the first one. The earlier version of this test warmed the cache
// before asserting, which measured the wrong request.
func TestAFilterThatMatchesNothingReadsNoSessionsOneAtATime(t *testing.T) {
	s := newTestServer(t)
	seedTaskSessions(t, s, 60)
	counter := countSessionLoads(t, s)

	w := do(t, s, "GET", "/api/tasks?limit=20&status=blocked", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if n := counter.gets.Load(); n != 0 {
		t.Errorf("冷缓存、匹配不到时读了 %d 次完整会话 —— 最坏路径仍是每会话一次加载", n)
	}

	var body taskListBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tasks) != 0 {
		t.Errorf("过滤条件匹配不到，却返回了 %d 个任务", len(body.Tasks))
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

// The scan ceiling, at its real size, cold.
//
// taskScanMax is 600 sessions, and a filter matching nothing walks all of
// them. This seeds past the ceiling so the walk actually reaches it rather
// than running out of sessions, and asserts on the first request — no warm-up.
//
// Two things have to hold, and only the first is about the cache: the number
// of full session reads is zero, and the number of round trips is a function
// of the page size rather than of how many sessions were walked. The second
// is why this also asserts a wall-clock bound: a per-session query that
// returned instantly on a local file would satisfy the first and still be
// twelve seconds against PostgreSQL.
func TestTheScanCeilingIsFlatAndCold(t *testing.T) {
	s := newTestServer(t)
	seedTaskSessions(t, s, 620)
	counter := countSessionLoads(t, s)

	start := time.Now()
	w := do(t, s, "GET", "/api/tasks?limit=20&status=blocked", "")
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if n := counter.gets.Load(); n != 0 {
		t.Errorf("扫到上限时读了 %d 次完整会话", n)
	}
	// Loose, because this runs on whatever machine CI has. It is here to
	// catch a return to per-session work, which was seconds and not
	// milliseconds — not to pin a number.
	if elapsed > 2*time.Second {
		t.Errorf("扫到上限用了 %s —— 像是又变成逐会话的活了", elapsed.Round(time.Millisecond))
	}
	t.Logf("620 个会话、过滤不中、冷缓存：%s", elapsed.Round(time.Millisecond))
}

// ADK ships no index on its own tables, and both queries this repo runs
// against them scan without one.
//
// events is keyed (id, app_name, user_id, session_id) with id first, so "the
// events of this session" has no usable prefix; sessions has no index on
// update_time, so "newest first" sorts the table. With 650 sessions the task
// scan's fifteen pages took 3.8 seconds on SQLite and 3.1 on PostgreSQL —
// essentially all of the worst path, and none of it in the code this repo
// wrote.
func TestTheIndexesOnADKsTablesExist(t *testing.T) {
	s := newTestServer(t)
	newSession(t, s, "anything") // opens the service, which creates the tables
	db, err := s.engine().StateDB()
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]string{
		"events":   "idx_events_session",
		"sessions": "idx_sessions_recent",
	} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, want).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("%s 上缺少 %s —— 任务列表会退回全表扫描", table, want)
		}
	}
}
