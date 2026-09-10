package session

import (
	"context"
	"github.com/jelly-agent/jelly-agent/internal/storage"
	"os"
	"path/filepath"
	"sync"
	"testing"

	adksession "google.golang.org/adk/session"
)

// TestSQLiteRoundTrip verifies the pure-Go SQLite session service can create,
// retrieve and list sessions (schema migration runs on open).
func TestSQLiteRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	svc, _, err := New(dbPath)
	if err != nil {
		t.Fatalf("session store: %v", err)
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
	svc2, _, err := New(dbPath)
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

// ADK's store must use this repo's pool, and must be releasable.
//
// Handed a DSN, GORM opens its own connection with no limit set — one more
// unbounded pool against a server whose default max_connections is 100 — and
// ADK's Service interface has no Close, so nothing could release it. A config
// reload then abandoned one per reload, on the connection every page uses.
func TestSessionStoreUsesABoundedPoolItCanRelease(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN")
	}
	exclusive(t, dsn)
	before := backendCount(t, dsn)

	svc, closeSvc, err := New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if svc == nil || closeSvc == nil {
		t.Fatal("New 没有交出可以释放的东西")
	}

	// Enough concurrent work that an unbounded pool would open a connection
	// per goroutine; a bounded one stays at its limit.
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.Get(context.Background(), &adksession.GetRequest{
				AppName: "jelly-agent", UserID: "local-user", SessionID: "nope",
			})
		}()
	}
	wg.Wait()

	peak := backendCount(t, dsn) - before
	if peak > 8 {
		t.Errorf("32 个并发查询开了 %d 条连接 —— 池子没有上限", peak)
	}

	if err := closeSvc(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if after := backendCount(t, dsn) - before; after > 1 {
		t.Errorf("释放之后还留着 %d 条连接", after)
	}
}

// backendCount asks PostgreSQL how many connections this database has.
func backendCount(t *testing.T, dsn string) int {
	t.Helper()
	db, err := storage.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
