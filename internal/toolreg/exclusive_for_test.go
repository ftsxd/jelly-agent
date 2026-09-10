package toolreg

import (
	"context"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// exclusive gives this test the shared PostgreSQL to itself for its duration.
//
// `go test ./...` runs packages in parallel and several of them exercise the
// PostgreSQL paths, so they arrive at one development database together — and
// ADK's AutoMigrate, which several of them trigger, panics inside GORM's
// migrator when two alter the same table at once.
func exclusive(t *testing.T, dsn string) {
	t.Helper()
	db, err := storage.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	release, err := storage.LockExclusively(context.Background(), db)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { release(); db.Close() })
}
