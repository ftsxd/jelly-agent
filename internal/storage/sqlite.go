package storage

import (
	"database/sql"
	"errors"
	"fmt"

	sqlite "github.com/glebarez/go-sqlite"
)

// Everything SQLite-specific. A second dialect goes in a file beside this one
// and the four symbols below become methods on it; nothing outside this
// package has to change, which is the entire point of the package.

const driverName = "sqlite"

// connLimit is one on purpose, and it is load-bearing.
//
// Every store's correctness currently rests on it: with one connection a
// statement cannot interleave with another from the same handle, so the
// read-modify-write sequences scattered through the stores are atomic without
// anybody having written a transaction. Raising it here would break them
// quietly. Transactions have to come first — see the storage migration notes.
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
//   - synchronous=NORMAL: the standard pairing with WAL. Two of the six
//     openers set it and four did not; one setting for one file is the honest
//     resolution, and NORMAL is what the two write-heavy stores chose.
func configure(db *sql.DB) error {
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
func hasColumn(db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&n)
	return n > 0, err
}

// SQLITE_CONSTRAINT_UNIQUE / _PRIMARYKEY. Two extended codes because SQLite
// reports a clash on a UNIQUE index and one on a PRIMARY KEY separately, and
// callers retrying an allocation care about neither distinction.
const (
	sqliteConstraintUnique     = 2067
	sqliteConstraintPrimaryKey = 1555
)

// isUniqueViolation reports whether err is a uniqueness clash.
//
// The equivalent on PostgreSQL is SQLSTATE 23505 (verified against 16.6),
// which is why this is not a
// strings.Contains at the call site. Callers use it to retry an allocation
// that lost a race, so a false negative turns a retryable conflict into a
// failed tool result, and a false positive retries something that will never
// succeed — the code, not the message.
func isUniqueViolation(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code() == sqliteConstraintUnique || e.Code() == sqliteConstraintPrimaryKey
}
