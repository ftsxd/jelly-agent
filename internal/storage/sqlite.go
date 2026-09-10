package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sqlite "github.com/glebarez/go-sqlite" // registers the "sqlite" driver
)

// Everything SQLite-specific.

type sqliteDialect struct{}

func (sqliteDialect) driver() string { return "sqlite" }

func (sqliteDialect) prepare(ref string) error {
	if dir := filepath.Dir(ref); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("storage: create db dir %s: %w", dir, err)
		}
	}
	return nil
}

// rebind is the identity: `?` is already SQLite's placeholder.
//
// It still goes through the same seam every other dialect uses, so the
// migration of several hundred call sites onto the wrapper could be done and
// verified without any dialect behaviour changing at the same time.
func (sqliteDialect) rebind(q string) string { return q }

// connLimit is one on purpose, and it is load-bearing.
//
// Several stores' correctness rests on it: with one connection a statement
// cannot interleave with another from the same handle, so read-modify-write
// sequences are atomic without anybody having written a transaction. Raising
// it would break them quietly, which is why the transactions come first.
//
// It is also not what it looks like from any single store. Six callers open
// the same state.db, so the process holds six pools of one, not one pool. WAL
// is what makes that survivable.
const connLimit = 1

// configure applies the settings every handle on a shared SQLite file needs.
//
//   - WAL: readers do not block the writer. Without it, one store reading
//     blocks another store writing, on the same file, and the writer fails.
//   - busy_timeout: a brief write lock is waited out instead of returning
//     "database is locked" — which, on the metrics recorder, silently drops
//     exactly the rows a busy run produces.
//   - synchronous=NORMAL: the standard pairing with WAL. Two of the seven
//     openers set it and five did not; one setting for one file is the honest
//     resolution, and NORMAL is what the write-heavy ones chose.
func (sqliteDialect) configure(db *sqlDB) error {
	db.SetMaxOpenConns(connLimit)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			return fmt.Errorf("storage: %s: %w", pragma, err)
		}
	}
	return nil
}

// hasColumn reports whether table already has a column of that name.
//
// pragma_table_info is SQLite's; the equivalent elsewhere is a query against
// information_schema.columns, which is why this is not inlined at the three
// call sites it replaced.
func (sqliteDialect) hasColumn(db *DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&n)
	return n > 0, err
}

// SQLITE_CONSTRAINT_UNIQUE / _PRIMARYKEY. Two extended codes because SQLite
// reports a clash on a UNIQUE index and one on a PRIMARY KEY separately, and
// no caller cares about the distinction.
const (
	sqliteConstraintUnique     = 2067
	sqliteConstraintPrimaryKey = 1555
)

// isUniqueViolation reports whether err is a uniqueness clash.
//
// By code, not by message: the equivalent on PostgreSQL is SQLSTATE 23505
// (verified against 16.6), and a message match would quietly stop working when
// the database changes.
func (sqliteDialect) isUniqueViolation(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code() == sqliteConstraintUnique || e.Code() == sqliteConstraintPrimaryKey
}

// sqlDB is the native handle, aliased so dialect.configure can take one
// without every other file in the package naming database/sql.
type sqlDB = sql.DB

// createsOwnSchema is true: a SQLite file has no migration step. It appears
// when the process starts, so each store creates what it needs — which is what
// makes a fresh install and a test's t.TempDir() work with no setup.
func (sqliteDialect) createsOwnSchema() bool { return true }

func (sqliteDialect) hasTable(db *DB, table string) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type IN ('table','view') AND name = ?`,
		table).Scan(&n)
	return n > 0, err
}

func (sqliteDialect) epochSeconds(c string) string {
	return `CAST(strftime('%s', ` + c + `) AS INTEGER)`
}

// isMissingTable matches the driver's message, which is all SQLite offers: the
// extended code for it (SQLITE_ERROR) says nothing more than "error".
func (sqliteDialect) isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

func (sqliteDialect) columns(db *DB, table string) ([]string, error) {
	return scanStrings(db, `SELECT name FROM pragma_table_info(?) ORDER BY name`, table)
}

func (sqliteDialect) columnTypes(db *DB, table string) (map[string]string, error) {
	return scanPairs(db, `SELECT name, type FROM pragma_table_info(?)`, table)
}

// primaryKey reads pragma_table_info's pk column, which is 0 for a column
// outside the key and 1-based position inside it.
func (sqliteDialect) primaryKey(db *DB, table string) ([]string, error) {
	return scanStrings(db, `SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, table)
}

// lockKey does nothing: SQLite admits one writer at a time, so a transaction
// that has begun writing already owns every key there is.
func (sqliteDialect) lockKey(context.Context, *Tx, int64) error { return nil }

// rowLocks is false: SQLite serialises writers, so there is nothing for a row
// lock to prevent — and FOR UPDATE is not syntax it accepts.
func (sqliteDialect) rowLocks() bool { return false }
