package storage

import (
	"context"
	"fmt"
)

// exclusiveTestLock is one arbitrary but fixed number, so every caller of
// LockExclusively contends for the same lock.
const exclusiveTestLock = 0x6a656c6c // "jell"

// LockExclusively blocks until this process has the shared database to itself,
// and returns the release.
//
// For tests. Several packages exercise the PostgreSQL paths and `go test ./...`
// runs packages in parallel, so they arrive at one development database
// together — and ADK's AutoMigrate, which each of them triggers, panics inside
// GORM's migrator when two of them alter the same table at once. That is a
// property of sharing one database, not a bug in any of them.
//
// A PostgreSQL advisory lock rather than a documented `-p 1`: the flag is a
// footnote that has to be remembered, and forgetting it produces a panic deep
// in a dependency rather than a message about test setup.
//
// On SQLite there is nothing to coordinate — every test has its own file — so
// this is a no-op and the release does nothing.
func LockExclusively(ctx context.Context, db *DB) (func(), error) {
	if db.Kind() != KindPostgres {
		return func() {}, nil
	}
	if _, err := db.ExecContext(ctx, `SELECT pg_advisory_lock(?)`, exclusiveTestLock); err != nil {
		return nil, fmt.Errorf("storage: take exclusive lock: %w", err)
	}
	return func() {
		// Best effort: the connection closing releases it anyway, and a
		// failure here must not fail the caller's own cleanup.
		db.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(?)`, exclusiveTestLock)
	}, nil
}
