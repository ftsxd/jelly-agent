// Package storage opens the databases this process shares.
//
// It exists because the same twenty lines were copied into seven openers —
// driver name, pool size, PRAGMAs, and a column probe — and every one of those
// twenty lines is a thing SQLite does differently from any other database. As
// long as they were spread across internal/record, internal/metrics,
// internal/memory, internal/task, internal/schedule and internal/session,
// "support PostgreSQL" meant finding and changing all seven, and getting one
// of them subtly wrong meant one store behaving differently from the rest on a
// Tuesday.
//
// Dialect-specific knowledge is exactly five things, and they all live here:
//
//  1. which driver to load, and how a reference names a database
//  2. what a placeholder looks like
//  3. how big the connection pool may be
//  4. what has to be set on a fresh connection (PRAGMAs, for SQLite)
//  5. how to ask whether a table has a column, and how a uniqueness clash
//     announces itself
//
// Callers get DB, Tx, Open and EnsureColumns, write their queries with `?`,
// and never name a driver.
package storage

import (
	"database/sql"
	"fmt"
	"strings"
)

// Open returns a configured handle to the database ref names.
//
// A postgres:// or postgresql:// ref selects PostgreSQL; anything without a
// URL scheme is a SQLite filesystem path. SQLite's parent directory is created
// if it does not exist, because every caller wanted that and only three of them
// had their own copy of it.
//
// The caller closes the handle.
func Open(ref string) (*DB, error) {
	if ref == "" {
		return nil, fmt.Errorf("storage: empty database reference")
	}
	d, err := dialectFor(ref)
	if err != nil {
		return nil, err
	}
	if err := d.prepare(ref); err != nil {
		return nil, err
	}
	native, err := sql.Open(d.driver(), ref)
	if err != nil {
		// Do not put ref in this error: a PostgreSQL URL may contain a password.
		return nil, fmt.Errorf("storage: open %s: %w", d.driver(), err)
	}
	if err := d.configure(native); err != nil {
		native.Close()
		return nil, err
	}
	return &DB{db: native, dialect: d}, nil
}

// dialectFor chooses by URL scheme. A value with no URL scheme is a SQLite
// path, preserving the existing API; a value that claims to be some other URL
// is rejected instead of becoming a surprisingly named SQLite file.
func dialectFor(ref string) (dialect, error) {
	if scheme, _, ok := strings.Cut(ref, "://"); ok {
		switch scheme {
		case "postgres", "postgresql":
			return postgresDialect{}, nil
		default:
			return nil, fmt.Errorf("storage: unsupported database scheme %q", scheme)
		}
	}
	return sqliteDialect{}, nil
}

// IsUniqueViolation reports whether err is a uniqueness clash — a UNIQUE index
// or a PRIMARY KEY rejecting a row that is already there.
//
// Not for retrying. Sequence numbers are handed out by a counter that blocks
// rather than collides, so a clash on one means an invariant is broken — a
// counter behind its table, or a second writer on the old path — and the
// caller uses this to say so instead of returning a bare driver error.
func IsUniqueViolation(err error) bool {
	return sqliteDialect{}.isUniqueViolation(err) || postgresDialect{}.isUniqueViolation(err)
}

// Column is one column a table is expected to have, and the DDL that adds it.
type Column struct {
	Name string
	DDL  string
}

// EnsureColumns adds each column the table does not already have.
//
// CREATE TABLE IF NOT EXISTS does nothing to a table that already exists, so a
// column added to a schema literal is missing from every database created
// before it — and the failure lands at runtime, as a bad INSERT, on a machine
// that had been running fine for months. Three stores learned this separately
// and wrote three copies of this loop.
//
// Adding is skipped rather than attempted-and-tolerated: a duplicate-column
// error is not distinguishable, across dialects, from the errors worth seeing.
func EnsureColumns(db *DB, table string, cols []Column) error {
	for _, col := range cols {
		has, err := db.dialect.hasColumn(db, table, col.Name)
		if err != nil {
			return fmt.Errorf("storage: inspect %s.%s: %w", table, col.Name, err)
		}
		if has {
			continue
		}
		if _, err := db.Exec(col.DDL); err != nil {
			return fmt.Errorf("storage: add %s.%s: %w", table, col.Name, err)
		}
	}
	return nil
}

// ChunkSize is how many placeholders one statement may carry.
//
// Every database caps it: SQLite refuses at 32,767 (measured, not quoted — the
// documented default has changed twice), PostgreSQL's wire protocol stops at
// 65,535. A list of session ids comes straight from the client with no bound
// on its length, so a single IN (…) is a wall somebody eventually hits, and it
// fails the whole delete rather than part of it.
//
// Well under the lower cap, because a statement is not the only thing carrying
// parameters and a margin costs nothing at these sizes.
const ChunkSize = 900

// ForEachChunk calls fn with successive slices of items, none longer than
// ChunkSize, stopping at the first error.
//
// Deliberately not opening a transaction. Whether the chunks have to agree
// with each other is the caller's question, not this function's: a delete
// wants an ordinary transaction so it is all-or-nothing, while a read that was
// one statement and became several needs a read-only repeatable-read one to
// keep its snapshot — and some callers need neither. Composing the two
// primitives says which was chosen; folding them together would hide it.
func ForEachChunk[T any](items []T, fn func([]T) error) error {
	for start := 0; start < len(items); start += ChunkSize {
		end := min(start+ChunkSize, len(items))
		if err := fn(items[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// Placeholders is "?,?,…" for n values, for building an IN (…) list.
func Placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*2)
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}

// RetryOnConflict runs fn until it succeeds, gives up, or fails for a reason
// retrying cannot fix.
//
// Retained only so this commit changes no behaviour while several hundred call
// sites move onto the wrapper. It goes away with the sequence counter: once
// numbers are handed out by something that blocks rather than collides, a
// uniqueness clash means an invariant is broken, and retrying it four times
// hides the signal instead of recovering from it — worse, when the allocation
// and the write share a transaction the rollback returns the counter too, so
// every retry asks for the same number again.
func RetryOnConflict[T any](attempts int, fn func() (T, error)) (T, error) {
	if attempts < 1 {
		attempts = 1
	}
	var (
		v   T
		err error
	)
	for i := 0; i < attempts; i++ {
		if v, err = fn(); err == nil || !IsUniqueViolation(err) {
			return v, err
		}
	}
	return v, err
}
