package memory

import (
	"context"
	"sync"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/storage"
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
		db, err := storage.Open(dsn)
		if err != nil {
			held--
			t.Fatal(err)
		}
		rel, err := storage.LockExclusively(context.Background(), db)
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
