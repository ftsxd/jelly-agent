// Package storage opens the databases this process shares.
//
// It exists because the same twenty lines were copied into six openers —
// driver name, pool size, PRAGMAs, and a column probe — and every one of those
// twenty lines is a thing SQLite does differently from any other database. As
// long as they are spread across internal/record, internal/metrics,
// internal/memory, internal/task and internal/session, "support MySQL" means
// finding and changing all six, and getting one of them subtly wrong means one
// store behaves differently from the rest on a Tuesday.
//
// So the dialect-specific knowledge is exactly four things, and they all live
// here:
//
//  1. which driver to load, and how a reference names a database
//  2. how big the connection pool may be
//  3. what has to be set on a fresh connection (PRAGMAs, for SQLite)
//  4. how to ask whether a table already has a column
//
// Callers get Open and EnsureColumns and never name a driver.
package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/glebarez/go-sqlite" // registers the "sqlite" driver
)

// Open returns a configured handle to the database ref names.
//
// Today ref is a filesystem path and the database is SQLite; the parent
// directory is created if it does not exist, because every caller wanted that
// and half of them had their own copy of it. When a networked database is
// added, ref grows a scheme and this is the only function that has to learn
// about it.
//
// The caller closes the handle. Note that six callers opening the same path
// today get six independent pools onto one file — see connLimit.
func Open(ref string) (*sql.DB, error) {
	if ref == "" {
		return nil, fmt.Errorf("storage: empty database reference")
	}
	if dir := filepath.Dir(ref); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("storage: create db dir %s: %w", dir, err)
		}
	}
	db, err := sql.Open(driverName, ref)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", ref, err)
	}
	if err := configure(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
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
func EnsureColumns(db *sql.DB, table string, cols []Column) error {
	for _, col := range cols {
		has, err := hasColumn(db, table, col.Name)
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
