package storage

import (
	"context"
	"fmt"
	"strings"
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
//
// It also refuses a database whose name does not say it is disposable. That
// is not tidiness: the suites behind this lock empty every table as setup and
// again as cleanup, so pointing them at a database somebody is using deletes
// what is in it. It happened — a deployment was moved onto the same
// PostgreSQL the tests were using, one `go test` wiped its sessions, and only
// an untouched SQLite source made that recoverable. A name is a weak fence,
// but it is checked before the first DELETE rather than after it, and
// "jelly_test" is not a name anything gets pointed at by accident.
func LockExclusively(ctx context.Context, db *DB) (func(), error) {
	if db.Kind() != KindPostgres {
		return func() {}, nil
	}
	if err := refuseNonTestDatabase(db); err != nil {
		return nil, err
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

// refuseNonTestDatabase rejects a target whose name does not mark it
// disposable.
//
// The name is read from the server rather than parsed out of the DSN: a DSN
// can carry the database in the path, in a `dbname=` parameter, or in
// PGDATABASE, and a check that only understands one of those is a check that
// can be walked around without meaning to.
func refuseNonTestDatabase(db *DB) error {
	var name string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&name); err != nil {
		return fmt.Errorf("storage: 读不到目标库名，拒绝在上面跑会清空数据的测试: %w", err)
	}
	if strings.Contains(strings.ToLower(name), "test") {
		return nil
	}
	return fmt.Errorf("storage: 拒绝在 %q 上跑测试 —— PG 测试会清空每一张表，"+
		"而这个库名没说明它是可丢弃的。请把 JELLY_PG_DSN 指向一个名字里带 test 的库"+
		"（建库并跑一遍 migrations/postgres/0001_init.sql 即可）；"+
		"生产库走 storage.dsn，不要用同一个", name)
}
