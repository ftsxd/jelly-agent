package server

import (
	"context"
	"testing"

	adksession "google.golang.org/adk/session"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
)

// A session whose name is its identity must keep that name.
//
// The scheduled-task path called resolveSession, which renames an unknown
// session to a fresh "web-…" id — and then discarded the return, so it ran
// against a name that still did not exist. With no AutoCreateSession on the
// runner, every scheduled task failed on a clean database and left an orphan
// session behind. Both halves are asserted here: the name survives, and the
// session is really there afterwards.
func TestEnsureSessionKeepsTheNameItWasGiven(t *testing.T) {
	s := newTestServer(t)
	svc, err := s.engine().NewSessionService()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	const id = "schedule-nightly"
	if err := ensureSession(ctx, svc, id); err != nil {
		t.Fatal(err)
	}
	resp, err := svc.Get(ctx, &adksession.GetRequest{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: id,
	})
	if err != nil || resp.Session == nil {
		t.Fatalf("the session was not created under its own name: %v", err)
	}
	if got := resp.Session.ID(); got != id {
		t.Errorf("session id = %q, want %q", got, id)
	}

	// Idempotent: a second run must reuse it, not create a second one.
	if err := ensureSession(ctx, svc, id); err != nil {
		t.Fatal(err)
	}
	again, err := svc.Get(ctx, &adksession.GetRequest{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: id,
	})
	if err != nil || again.Session == nil {
		t.Fatalf("the session vanished on the second ensure: %v", err)
	}
}

// resolveSession keeps its own behaviour: a browser asking for a session that
// is gone should get a new conversation, not an error.
func TestResolveSessionStillRenamesForTheBrowser(t *testing.T) {
	s := newTestServer(t)
	svc, err := s.engine().NewSessionService()
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.resolveSession(context.Background(), svc, "does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	if got == "does-not-exist" {
		t.Error("resolveSession returned an id it never created")
	}
	if got == "" {
		t.Error("no session id")
	}
}

func TestEnsureSessionRejectsAnEmptyName(t *testing.T) {
	s := newTestServer(t)
	svc, err := s.engine().NewSessionService()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureSession(context.Background(), svc, "  "); err == nil {
		t.Error("an empty session name was accepted")
	}
}

// The regression test for the bug that made scheduled tasks impossible.
//
// runScheduledAgent called resolveSession and threw the result away.
// resolveSession renames an unknown session to a fresh "web-…" id, so the
// first run of any task created a throwaway session and then ran against
// "schedule-<name>", which still did not exist — and the runner has no
// AutoCreateSession, so it failed. Every attempt failed, forever, and left an
// orphan session behind.
//
// The run below cannot complete (the provider points at a dead URL), which is
// fine and is the point: what is asserted is the session it prepared, not the
// answer it failed to get.
func TestAScheduledRunPreparesItsOwnNamedSession(t *testing.T) {
	s := newTestServer(t)
	svc, err := s.engine().NewSessionService()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, _, _ = s.runScheduledAgent(ctx, s.engine(), scheduleTaskFixture(), "巡检一下")

	// The session exists under the name the scheduler chose.
	resp, err := svc.Get(ctx, &adksession.GetRequest{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: "schedule-nightly",
	})
	if err != nil || resp.Session == nil {
		t.Fatalf("the scheduled session was never created under its own name: %v", err)
	}

	// And no throwaway was created alongside it. An orphan per attempt was the
	// other half of the bug.
	ids, err := listSessionIDs(t, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id != "schedule-nightly" {
			t.Errorf("an extra session %q was created; the scheduler renamed instead of naming", id)
		}
	}
}

func scheduleTaskFixture() config.ScheduleTask {
	return config.ScheduleTask{Name: "nightly", Cron: "0 3 * * *", Prompt: "巡检", Enabled: true}
}

func listSessionIDs(t *testing.T, s *Server) ([]string, error) {
	t.Helper()
	return jellysession.AllIDs(stateDBOf(t, s), engine.AppName, engine.UserID)
}
