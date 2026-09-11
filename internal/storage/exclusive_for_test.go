package storage

import (
	"context"
	"sync"
	"testing"
)

// held is this process's own view of the lock.
//
// pg_advisory_lock is per connection, so a second call from a different
// connection in the same process blocks on the first — which is a deadlock,
// not protection, when one helper calls another that also locks. Taking it
// once per process and counting is what makes the helper safe to nest.
var (
	heldMu  sync.Mutex
	held    int
	release func()
)

// exclusive gives this test the shared PostgreSQL to itself for its duration.
//
// The same helper the other packages have, in-package here because
// LockExclusively lives in this one. Without it this package was the one
// exception that connected while another package was counting connections —
// which is what made the session pool test flaky.
//
// `go test ./...` runs packages in parallel and several exercise the
// PostgreSQL paths, so they meet at one development database — and ADK's
// AutoMigrate, which several of them trigger, panics inside GORM's migrator
// when two alter the same table at once.
func exclusive(t *testing.T, dsn string) {
	t.Helper()
	heldMu.Lock()
	defer heldMu.Unlock()
	held++
	if held == 1 {
		db, err := Open(dsn)
		if err != nil {
			held--
			t.Fatal(err)
		}
		rel, err := LockExclusively(context.Background(), db)
		if err != nil {
			db.Close()
			held--
			t.Fatal(err)
		}
		release = func() { rel(); db.Close() }
	}
	t.Cleanup(func() {
		heldMu.Lock()
		defer heldMu.Unlock()
		held--
		if held == 0 && release != nil {
			release()
			release = nil
		}
	})
}
