package server

import (
	"net/http"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/task"
)

// The same rule at the edge, so a client is told rather than silently ignored.
func TestAChatCannotClaimAnotherSessionsTask(t *testing.T) {
	s := newTestServer(t)
	newSession(t, s, "web-mine")
	body := `{"session_id":"web-mine","message":"继续","task_id":"web-someone-else/inv-1"}`
	w := do(t, s, "POST", "/api/chat/stream", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	links, err := task.OfSession(s.engine().SessionDBPath(), "web-mine")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 {
		t.Errorf("被拒绝的请求仍然写下了归属: %v", links)
	}
}
