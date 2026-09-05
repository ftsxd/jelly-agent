package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/record"
)

// Expired and missing must not read the same to a person.
//
// "Not found" says look again — a wrong id, another session, a call whose
// delivery never landed. "Expired" says the bytes are gone and the only way to
// get them is to run the tool again. Before this the two were one branch, and
// expiry actually fell through to a 500 with a raw store error in it.
func TestExpiredAndMissingResultsAreDifferentAnswers(t *testing.T) {
	s := newTestServer(t)
	store, err := s.engine().Records()
	if err != nil {
		t.Fatal(err)
	}
	sc := record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-1"}

	if _, err := store.Put(t.Context(), record.Record{
		Scope: sc, InvocationID: "inv-1", CallID: "c-old",
		Tool: "get_logs", At: time.Now().Add(-30 * 24 * time.Hour),
		Payload: []byte("ancient logs"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), record.Record{
		Scope: sc, InvocationID: "inv-1", CallID: "c-live",
		Tool: "get_logs", At: time.Now(), Payload: []byte("today's logs"),
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.Sweep(t.Context(), 7*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}

	for _, tc := range []struct {
		name, call string
		want       int
	}{
		{"过期", "c-old", http.StatusGone},
		{"从未存在", "c-nope", http.StatusNotFound},
		{"仍可读", "c-live", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, s, "GET", "/api/sessions/web-1/results/"+tc.call, "")
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
	if _, err := store.Put(t.Context(), record.Record{
		Scope:        record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: "web-1"},
		InvocationID: "inv-1", CallID: "c1", Tool: "get_logs", At: time.Now(),
		Payload: []byte(`{"output":` + jsonString(body) + `}`),
	}); err != nil {
		t.Fatal(err)
	}

	w := do(t, s, "GET", "/api/sessions/web-1/results/c1/search?q=TIMEOUT&limit=5", "")
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
	if w := do(t, s, "GET", "/api/sessions/web-1/results/c1/search?q=a%28b", ""); w.Code != http.StatusBadRequest {
		t.Errorf("an unparseable pattern gave %d, want 400", w.Code)
	}
	if w := do(t, s, "GET", "/api/sessions/web-1/results/c1/search?q=", ""); w.Code != http.StatusBadRequest {
		t.Errorf("an empty pattern gave %d, want 400", w.Code)
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
