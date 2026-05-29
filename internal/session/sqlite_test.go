package session

import (
	"context"
	"path/filepath"
	"testing"

	adksession "google.golang.org/adk/session"
)

// TestSQLiteRoundTrip verifies the pure-Go SQLite session service can create,
// retrieve and list sessions (schema migration runs on open).
func TestSQLiteRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	svc, err := NewSQLite(dbPath)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}

	ctx := context.Background()
	const (
		app  = "jelly-agent"
		user = "u1"
		sid  = "s1"
	)
	if _, err := svc.Create(ctx, &adksession.CreateRequest{AppName: app, UserID: user, SessionID: sid}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.Get(ctx, &adksession.GetRequest{AppName: app, UserID: user, SessionID: sid})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Session == nil || got.Session.ID() != sid {
		t.Fatalf("Get returned %+v, want session id %q", got.Session, sid)
	}

	list, err := svc.List(ctx, &adksession.ListRequest{AppName: app, UserID: user})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Sessions) != 1 {
		t.Fatalf("List returned %d sessions, want 1", len(list.Sessions))
	}

	// Reopen the same file: data should persist across service instances.
	svc2, err := NewSQLite(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	list2, err := svc2.List(ctx, &adksession.ListRequest{AppName: app, UserID: user})
	if err != nil {
		t.Fatalf("List after reopen: %v", err)
	}
	if len(list2.Sessions) != 1 {
		t.Fatalf("after reopen got %d sessions, want 1 (not persisted)", len(list2.Sessions))
	}
}
