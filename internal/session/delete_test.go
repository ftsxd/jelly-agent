package session

import (
	"context"
	"path/filepath"
	"testing"

	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"
)

// TestDeleteSessionsCascadesEvents verifies deleting a session removes its
// events too (no orphans), and that PurgeOrphanEvents sweeps pre-existing
// leftovers.
func TestDeleteSessionsCascadesEvents(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	svc, err := NewSQLite(dbPath)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	ctx := context.Background()
	const app, user = "jelly-agent", "local-user"

	mk := func(sid string, events int) {
		resp, err := svc.Create(ctx, &adksession.CreateRequest{AppName: app, UserID: user, SessionID: sid})
		if err != nil {
			t.Fatalf("Create %s: %v", sid, err)
		}
		for j := 0; j < events; j++ {
			ev := adksession.NewEvent("inv")
			ev.Author = "user"
			ev.Content = genai.NewContentFromText("hi", genai.RoleUser)
			if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
				t.Fatalf("AppendEvent %s: %v", sid, err)
			}
		}
	}
	mk("keep", 2)
	mk("drop", 3)

	deleted, err := DeleteSessions(dbPath, app, user, []string{"drop", "missing"})
	if err != nil {
		t.Fatalf("DeleteSessions: %v", err)
	}
	if deleted != 1 { // "missing" is a no-op
		t.Errorf("deleted=%d, want 1", deleted)
	}

	// "drop" and its events are gone; "keep" untouched.
	rows, total, err := ListPage(dbPath, app, user, 50, 0)
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].ID != "keep" {
		t.Fatalf("after delete rows=%+v total=%d, want only keep", rows, total)
	}
	if rows[0].Events != 2 {
		t.Errorf("keep events=%d, want 2", rows[0].Events)
	}

	// No orphans should remain (delete removed drop's events).
	if n, err := PurgeOrphanEvents(dbPath); err != nil || n != 0 {
		t.Errorf("PurgeOrphanEvents after clean delete = (%d, %v), want (0, nil)", n, err)
	}

	// Simulate a legacy orphan: delete only the session row, leaving events.
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM sessions WHERE id = ?`, "keep"); err != nil {
		t.Fatalf("orphan setup: %v", err)
	}
	db.Close()

	n, err := PurgeOrphanEvents(dbPath)
	if err != nil {
		t.Fatalf("PurgeOrphanEvents: %v", err)
	}
	if n != 2 { // keep's 2 events are now orphaned
		t.Errorf("purged=%d, want 2", n)
	}
}
