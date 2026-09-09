package session

import (
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// stateDB opens the shared state database for a test and creates this
// package's schema, mirroring what engine.StateDB does for the process.
//
// It exists because the stores here now take a handle rather than a path: they
// used to open one per call, which is free against a local file and a full
// connection handshake against PostgreSQL.
func stateDB(t *testing.T, path string) *storage.DB {
	t.Helper()
	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}
