package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	adksession "google.golang.org/adk/session"

	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/metrics"
	"github.com/jelly-agent/jelly-agent/internal/record"
	"github.com/jelly-agent/jelly-agent/internal/task"
)

// Expired and missing must not read the same to a person.
//
// "Not found" says look again — a wrong id, another session, a call whose
// delivery never landed. "Expired" says the bytes are gone and the only way to
// get them is to run the tool again. Before this the two were one branch, and
// expiry actually fell through to a 500 with a raw store error in it.
func TestExpiredAndMissingResultsAreDifferentAnswers(t *testing.T) {
	s := newTestServer(t)
	newSession(t, s, "web-1")
	store, err := s.engine().Records()
	if err != nil {
		t.Fatal(err)
	}
	sc := record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-1"}

	old, err := store.Put(t.Context(), record.Record{
		Scope: sc, InvocationID: "inv-1", CallID: "c-old",
		Tool: "get_logs", At: time.Now().Add(-30 * 24 * time.Hour),
		Payload: []byte("ancient logs"),
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.Put(t.Context(), record.Record{
		Scope: sc, InvocationID: "inv-1", CallID: "c-live",
		Tool: "get_logs", At: time.Now(), Payload: []byte("today's logs"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := store.Sweep(t.Context(), 7*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}

	for _, tc := range []struct {
		name, ref string
		want      int
	}{
		{"过期", old, http.StatusGone},
		{"从未存在", "e99", http.StatusNotFound},
		{"仍可读", live, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, s, "GET", "/api/sessions/web-1/results/"+tc.ref, "")
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusGone && !contains(w.Body.String(), "已过期") {
				t.Errorf("body does not say it expired: %s", w.Body.String())
			}
			if tc.want == http.StatusNotFound && contains(w.Body.String(), "已过期") {
				t.Errorf("a missing result was reported as expired: %s", w.Body.String())
			}
		})
	}
}

// Search over a stored result is the same Store.Search the model's tool uses,
// so the two cannot disagree about what a match is or how many there were.
func TestSearchingAStoredResultOverHTTP(t *testing.T) {
	s := newTestServer(t)
	newSession(t, s, "web-1")
	store, err := s.engine().Records()
	if err != nil {
		t.Fatal(err)
	}
	body := ""
	for i := 0; i < 300; i++ {
		if i%10 == 0 {
			body += "2026-09-06 WARN payment-api TIMEOUT db-node-3\n"
		} else {
			body += "2026-09-06 INFO payment-api ok\n"
		}
	}
	ref, err := store.Put(t.Context(), record.Record{
		Scope:        record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-1"},
		InvocationID: "inv-1", CallID: "c1", Tool: "get_logs", At: time.Now(),
		Payload: []byte(`{"output":` + jsonString(body) + `}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	w := do(t, s, "GET", "/api/sessions/web-1/results/"+ref+"/search?q=TIMEOUT&limit=5", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	// 30 matches, five returned: the count is exact and the page is bounded,
	// which is the whole reason to search instead of reading.
	if !contains(w.Body.String(), `"total_matches":30`) {
		t.Errorf("body = %s", w.Body.String())
	}
	if !contains(w.Body.String(), `"total_exact":true`) {
		t.Errorf("the count was not reported as exact: %s", w.Body.String())
	}

	// A pattern the caller got wrong is theirs to fix, and they can only fix
	// it if told what was wrong with it.
	if w := do(t, s, "GET", "/api/sessions/web-1/results/"+ref+"/search?q=a%28b", ""); w.Code != http.StatusBadRequest {
		t.Errorf("an unparseable pattern gave %d, want 400", w.Code)
	}
	if w := do(t, s, "GET", "/api/sessions/web-1/results/"+ref+"/search?q=", ""); w.Code != http.StatusBadRequest {
		t.Errorf("an empty pattern gave %d, want 400", w.Code)
	}
	// A handle nothing was ever stored under is a miss, not somebody else's row.
	if w := do(t, s, "GET", "/api/sessions/web-1/results/e99/search?q=TIMEOUT", ""); w.Code != http.StatusNotFound {
		t.Errorf("an unknown handle gave %d, want 404", w.Code)
	}
}

// Two runs of one conversation both call their first tool c1. The console
// reads by handle for exactly this reason: addressing by call id made the
// second run's step show the first run's bytes, in the task centre, which is
// the one view that puts both runs on screen together.
func TestTwoRunsSharingACallIDReadBackSeparately(t *testing.T) {
	s := newTestServer(t)
	newSession(t, s, "web-1")
	store, err := s.engine().Records()
	if err != nil {
		t.Fatal(err)
	}
	sc := record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-1"}
	refs := map[string]string{}
	for _, run := range []struct{ inv, body string }{
		{"inv-1", "第一轮的日志"}, {"inv-2", "第二轮的日志"},
	} {
		ref, err := store.Put(t.Context(), record.Record{
			Scope: sc, InvocationID: run.inv, CallID: "c1",
			Tool: "get_logs", At: time.Now(), Payload: []byte(run.body),
		})
		if err != nil {
			t.Fatal(err)
		}
		refs[run.body] = ref
	}
	if len(refs) != 2 {
		t.Fatalf("两轮共用了一个句柄: %v", refs)
	}
	for body, ref := range refs {
		w := do(t, s, "GET", "/api/sessions/web-1/results/"+ref, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", ref, w.Code, w.Body.String())
		}
		if !contains(w.Body.String(), jsonString(body)[1:len(jsonString(body))-1]) {
			t.Errorf("%s 读到 %s，想要 %q", ref, w.Body.String(), body)
		}
	}
}

func contains(hay, needle string) bool { return len(hay) >= len(needle) && indexOf(hay, needle) >= 0 }

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func jsonString(s string) string {
	out := []byte{'"'}
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, string(r)...)
		}
	}
	return string(append(out, '"'))
}

// newSession creates the conversation a delivery belongs to.
//
// Required, not incidental: the delivery endpoints refuse a session that is
// not in the store, so that a handle held from before a delete cannot read
// back what the delete was supposed to remove.
func newSession(t *testing.T, s *Server, id string) {
	t.Helper()
	svc, err := s.engine().NewSessionService()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(t.Context(), &adksession.CreateRequest{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: id,
	}); err != nil {
		t.Fatal(err)
	}
}

// Deleting a conversation has to delete the conversation.
//
// It used to delete the events and the search index — and leave the two stores
// that actually hold the contents. tool_results keeps every call's raw payload
// byte for byte, tool_calls keeps what was called, and both are addressed by
// session id, so anyone holding a handle from before could still read back a
// conversation the user had asked to be gone. task_runs kept naming it too.
func TestDeletingASessionLeavesNothingReadable(t *testing.T) {
	s := newTestServer(t)
	newSession(t, s, "web-gone")
	store, err := s.engine().Records()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(t.Context(), record.Record{
		Scope:        record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-gone"},
		InvocationID: "inv-1", CallID: "c1", Tool: "get_logs", At: time.Now(),
		Payload: []byte("密码是 hunter2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr := s.engine().Metrics(); tr != nil {
		if err := tr.Recorder().Record(metrics.ToolCall{
			At: time.Now(), SessionID: "web-gone", InvocationID: "inv-1",
			CallID: "c1", Tool: "get_logs", OK: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	stateDB, err := s.engine().StateDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Link(stateDB, "web-gone/inv-1", "web-gone", "inv-2"); err != nil {
		t.Fatal(err)
	}
	// Readable before, so the assertion after the delete means something.
	if w := do(t, s, "GET", "/api/sessions/web-gone/results/"+ref, ""); w.Code != http.StatusOK {
		t.Fatalf("before delete: status = %d: %s", w.Code, w.Body.String())
	}

	if w := do(t, s, "DELETE", "/api/sessions/web-gone", ""); w.Code != http.StatusOK {
		t.Fatalf("delete: status = %d: %s", w.Code, w.Body.String())
	}

	// The endpoint no longer serves it...
	if w := do(t, s, "GET", "/api/sessions/web-gone/results/"+ref, ""); w.Code != http.StatusNotFound {
		t.Errorf("已删除会话的结果仍能读到: status = %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, s, "GET", "/api/sessions/web-gone/results/"+ref+"/search?q=hunter", ""); w.Code != http.StatusNotFound {
		t.Errorf("已删除会话的结果仍能搜索: status = %d", w.Code)
	}
	// ...and the bytes are gone from the store, not merely hidden by it.
	if _, err := store.ReadLabel(t.Context(), record.Scope{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-gone",
	}, ref, 0, 100); !errors.Is(err, record.ErrNotFound) {
		t.Errorf("原始返回内容还在库里: %v", err)
	}
	if tr := s.engine().Metrics(); tr != nil {
		rows, err := tr.ByInvocation("web-gone", "inv-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Errorf("调用记录还在: %+v", rows)
		}
	}
	links, err := task.OfSession(stateDB, "web-gone")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 {
		t.Errorf("任务归属还在: %v", links)
	}
}

// The handle is not the permission. A delivery whose conversation is not in
// the store is not served, even when its bytes are still sitting there —
// which is the state a half-finished delete leaves behind.
func TestADeliveryWithNoSessionIsNotServed(t *testing.T) {
	s := newTestServer(t)
	store, err := s.engine().Records()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(t.Context(), record.Record{
		Scope:        record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-orphan"},
		InvocationID: "inv-1", CallID: "c1", Tool: "get_logs", At: time.Now(),
		Payload: []byte("孤儿数据"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The bytes are there — this is exactly the case the check exists for.
	if _, err := store.ReadLabel(t.Context(), record.Scope{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-orphan",
	}, ref, 0, 100); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if w := do(t, s, "GET", "/api/sessions/web-orphan/results/"+ref, ""); w.Code != http.StatusNotFound {
		t.Errorf("没有会话的结果仍被提供: status = %d: %s", w.Code, w.Body.String())
	}
}

// A delete that could not remove the raw payloads must not answer ok.
//
// The first version logged each failure and reported success, so the operator
// would have had to be watching the log at the moment they clicked to learn
// that the conversation they deleted was still readable. That is the one
// outcome nobody can act on: the page says gone, the bytes say otherwise.
func TestADeleteThatCouldNotPurgeReportsFailure(t *testing.T) {
	s := newTestServer(t)
	newSession(t, s, "web-stuck")
	store, err := s.engine().Records()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), record.Record{
		Scope:        record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-stuck"},
		InvocationID: "inv-1", CallID: "c1", Tool: "get_logs", At: time.Now(),
		Payload: []byte("还在库里的东西"),
	}); err != nil {
		t.Fatal(err)
	}
	// The delivery store is unusable from here on — the shape a disk error or
	// a database that went away takes at this layer.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	w := do(t, s, "DELETE", "/api/sessions/web-stuck", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), "工具结果") {
		t.Errorf("报错没有说清楚是什么没删掉: %s", w.Body.String())
	}

	// The batch endpoint answers the same way, and still says how many
	// sessions it did remove.
	w = do(t, s, "POST", "/api/sessions/delete", `{"ids":["web-stuck"]}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("批量删除 status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), "deleted") {
		t.Errorf("批量删除没有回报已删除数量: %s", w.Body.String())
	}
}

// A client that walks away must not leave half a deleted conversation behind.
//
// The sessions are gone by the time the clean-up runs and none of it can be
// undone, so tying it to the request meant hitting stop — or just navigating
// away — cancelled it midway and left the raw payloads of a deleted
// conversation readable. With nobody to tell, either: the response could no
// longer be written.
func TestACancelledRequestStillFinishesTheDelete(t *testing.T) {
	s := newTestServer(t)
	newSession(t, s, "web-abandoned")
	store, err := s.engine().Records()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), record.Record{
		Scope:        record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-abandoned"},
		InvocationID: "inv-1", CallID: "c1", Tool: "get_logs", At: time.Now(),
		Payload: []byte("不该留下的东西"),
	}); err != nil {
		t.Fatal(err)
	}

	// The request arrives already cancelled, which is what the handler sees
	// from a browser that has gone.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequest("DELETE", "/api/sessions/web-abandoned", nil).WithContext(ctx)
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)

	if _, err := store.ReadLabel(t.Context(), record.Scope{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-abandoned",
	}, "e1", 0, 100); !errors.Is(err, record.ErrNotFound) {
		t.Errorf("调用方中途离开，原始返回内容就留在库里了: %v", err)
	}
}
