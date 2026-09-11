package session

import (
	"context"
	"github.com/jelly-agent/jelly-agent/internal/storage"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
func TestSessionStoreReleasesItsPool(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN")
	}
	exclusive(t, dsn)

	// Counted by application_name rather than by "how many backends does this
	// database have". The database is shared with every other package's PG
	// tests, and not all of them take the exclusive lock, so a plain count
	// measures whoever else happens to be connected at that instant — which
	// made this fail in the full suite and pass on its own.
	const tag = "jelly-pool-test"
	poolDSN := dsn + "&application_name=" + tag
	if !strings.Contains(dsn, "?") {
		poolDSN = dsn + "?application_name=" + tag
	}
	if before := backendCount(t, dsn, tag); before != 0 {
		t.Fatalf("开始之前就有 %d 条打着 %s 的连接", before, tag)
	}

	svc, closeSvc, err := New(poolDSN)
	if err != nil {
		t.Fatal(err)
	}
	if svc == nil || closeSvc == nil {
		t.Fatal("New 没有交出可以释放的东西")
	}

	// Some work, so the pool actually opens something to release.
	//
	// The bound itself is not asserted here any more, and that is deliberate:
	// these queries return immediately, so the pool reuses one connection and
	// the count stays low whether there is a limit or not — a version of this
	// test with SetMaxOpenConns(0) passed. The claim "the pool has a ceiling"
	// needs work slow enough to make connections pile up, which this service
	// has no way to issue; it lives in internal/storage, next to the policy.
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

	if opened := backendCount(t, dsn, tag); opened == 0 {
		t.Fatal("跑了 32 个查询，一条连接都没开 —— 这个测试没在测它以为在测的东西")
	}

	if err := closeSvc(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// PostgreSQL clears pg_stat_activity a moment after the client goes away,
	// so this is a wait rather than a single read: the claim is that the pool
	// releases its connections, not that the server notices instantly.
	deadline := time.Now().Add(5 * time.Second)
	for {
		after := backendCount(t, dsn, tag)
		if after == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("释放之后还留着 %d 条连接", after)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// backendCount asks PostgreSQL how many connections carry this
// application_name. Its own connection does not, so it never counts itself.
func backendCount(t *testing.T, dsn, tag string) int {
	t.Helper()
	db, err := storage.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_stat_activity
		 WHERE datname = current_database() AND application_name = ?`, tag).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
