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

// Kind names the database a reference selects.
type Kind string

const (
	KindSQLite   Kind = "sqlite"
	KindPostgres Kind = "postgres"
)

// KindOf reports which database ref selects, without opening it.
//
// Exported for the one caller that cannot go through DB: ADK's session store
// takes a GORM dialector rather than a database/sql handle, so
// internal/session has to choose one. Everything else uses Open and never asks.
//
// An unsupported scheme is an error here too, so a value that is rejected by
// Open is not silently accepted by the session store — which would leave the
// two halves of the same database pointed at different places.
func KindOf(ref string) (Kind, error) {
	d, err := dialectFor(ref)
	if err != nil {
		return "", err
	}
	if _, ok := d.(postgresDialect); ok {
		return KindPostgres, nil
	}
	return KindSQLite, nil
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

// Native hands out the underlying database/sql handle, and the only caller
// that may take it is internal/session.
//
// The rule everywhere else is that a native handle is never held outside this
// package, because a query issued on one skips the rebind — and `?` is
// SQLite's own placeholder, so that mistake is green in every test and fails
// only against PostgreSQL. ADK's session store is the exception on purpose: it
// takes a GORM dialector and writes its own SQL, so rebind does not apply to
// it at all.
//
// What it does need is this package's pool policy. Left to open its own
// connection from a DSN, GORM applies no limit — one more unbounded pool
// against a server whose default max_connections is 100, and one nothing can
// close because ADK's Service interface has no Close.
func (d *DB) Native() *sql.DB { return d.db }

// Kind names the database this handle is on.
//
// For the one difference that is not a spelling difference. Placeholders and
// strftime are the same query written two ways, and callers should never see
// them; full-text search is two different implementations — SQLite's FTS5
// MATCH with its own rank, PostgreSQL's ILIKE over a GIN trigram index with
// similarity() — and pretending otherwise would hide which one is running.
func (d *DB) Kind() Kind {
	if _, ok := d.dialect.(postgresDialect); ok {
		return KindPostgres
	}
	return KindSQLite
}

// IsMissingTable reports whether err is "that table does not exist".
//
// Several callers tolerate it: a store whose schema has not been created yet
// holds nothing, so listing or purging it is a no-op rather than a failure.
// Three of them matched SQLite's message text, which on PostgreSQL is not a
// match — a fresh deployment's first sessions-page load answered 500 with a
// SQL error in it, until something happened to open the ADK service first.
//
// Both dialects are asked, because the classification has to survive an error
// that came from a handle this function cannot see.
func IsMissingTable(err error) bool {
	return sqliteDialect{}.isMissingTable(err) || postgresDialect{}.isMissingTable(err)
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

// ApplySchema creates a store's tables, on a database that expects to be asked.
//
// The two dialects differ in who owns the schema, not just in how it is
// spelled:
//
//   - SQLite has no migration step. The file appears when the process starts,
//     so each store creates what it needs on open — which is what makes a
//     fresh install and a test's t.TempDir() work with no setup at all.
//   - PostgreSQL has one, and it is migrations/postgres/0001_init.sql. Running
//     the SQLite DDL against it would fail anyway (BLOB and INTEGER are not
//     PostgreSQL types), but the deeper reason is that a schema an operator
//     backs up and a schema a process invents on startup should not be the
//     same schema.
//
// So on PostgreSQL this checks rather than creates. A missing table becomes one
// clear error at startup naming the file to run, instead of a raw "relation
// does not exist" from whichever request happens to touch it first.
func ApplySchema(db *DB, ddl string, tables ...string) error {
	if db.dialect.createsOwnSchema() {
		if _, err := db.Exec(ddl); err != nil {
			return fmt.Errorf("storage: create schema: %w", err)
		}
		return nil
	}
	for _, t := range tables {
		has, err := db.dialect.hasTable(db, t)
		if err != nil {
			return fmt.Errorf("storage: inspect %s: %w", t, err)
		}
		if !has {
			return fmt.Errorf("storage: 表 %s 不存在。"+
				"PostgreSQL 的 schema 由迁移文件建，不由进程自己建 —— "+
				"先跑 migrations/postgres/0001_init.sql", t)
		}
	}
	return nil
}

// EpochSeconds renders a timestamp column as whole seconds since the epoch.
//
// Exported because a query has to be built with it, and the spelling is
// dialect-specific: SQLite has strftime('%s', c) and nothing else does. It
// belongs here for the same reason the placeholder does — a caller that writes
// the SQLite spelling inline works in every test and fails only on PostgreSQL.
func (d *DB) EpochSeconds(column string) string { return d.dialect.epochSeconds(column) }

// Columns lists a table's column names, sorted.
//
// For comparing the two schemas against each other. They are two independent
// definitions — the DDL a store runs on SQLite, and
// migrations/postgres/0001_init.sql — so a column added to one and not the
// other goes unnoticed until a query on the deployment that has the older one
// fails. ApplySchema checks that a table is there; nothing checked what is in
// it, and nothing at runtime can: a PostgreSQL deployment never runs the
// SQLite DDL, so only a test with both in front of it can compare them.
func Columns(db *DB, table string) ([]string, error) {
	return db.dialect.columns(db, table)
}

// scanStrings collects a single-column result.
func scanStrings(db *DB, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ColumnTypes maps a table's columns to the type names this database reports.
//
// For copying rows between databases without a third definition of the schema.
// The migrator reads what the target actually has rather than carrying its own
// list — the DDL a store runs and migrations/postgres/0001_init.sql are already
// two definitions, and a copier with a third would drift from both.
func ColumnTypes(db *DB, table string) (map[string]string, error) {
	return db.dialect.columnTypes(db, table)
}

// PrimaryKey lists a table's primary-key columns, in key order.
//
// For telling one row from another without a fourth definition of the schema.
// The migrator needs it to say *which* rows the target already had, which is
// the difference between "the copy skipped 3 rows" and "these 3 rows in the
// target say something different from the source".
//
// Empty means the table has no primary key, which the caller has to handle
// rather than treat as an error: a table without one has no way to match a
// source row to a target row at all.
func PrimaryKey(db *DB, table string) ([]string, error) {
	return db.dialect.primaryKey(db, table)
}

// scanPairs collects a two-column result into a map.
func scanPairs(db *DB, query string, args ...any) (map[string]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// HasIdentitySequences says whether generated columns are backed by a
// separate counter that a copied row does not advance.
//
// PostgreSQL's identity columns are; SQLite's AUTOINCREMENT reads MAX(rowid),
// so a row copied with an explicit id moves it by existing.
func (d *DB) HasIdentitySequences() bool { return d.Kind() == KindPostgres }
