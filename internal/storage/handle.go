package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// DB is a handle that knows its dialect.
//
// It deliberately does not embed *sql.DB. Embedding would let any call site
// reach the native ExecContext and skip the rebind — and that mistake is
// invisible: `?` is SQLite's own placeholder, so the query runs fine in every
// test and fails only against PostgreSQL, in production. Not embedding turns
// "forgot to rebind" from a runtime error into a compile error.
//
// For the same reason there is no Raw(). ADK's session store takes a GORM
// dialector rather than a handle from here, so nothing legitimately needs one.
type DB struct {
	db      *sql.DB
	dialect dialect
}

// Tx is a transaction on a DB. It carries its parent's dialect rather than
// choosing one, so a statement cannot be rebound one way outside a transaction
// and another way inside it.
type Tx struct {
	tx      *sql.Tx
	dialect dialect
}

// Every method below rebinds before delegating. Written out rather than
// generated because the list is short and the indirection would hide the one
// thing worth seeing at each of them.

func (d *DB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.db.ExecContext(ctx, d.dialect.rebind(q), args...)
}
func (d *DB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return d.db.QueryContext(ctx, d.dialect.rebind(q), args...)
}
func (d *DB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return d.db.QueryRowContext(ctx, d.dialect.rebind(q), args...)
}
func (d *DB) Exec(q string, args ...any) (sql.Result, error) {
	return d.ExecContext(context.Background(), q, args...)
}
func (d *DB) Query(q string, args ...any) (*sql.Rows, error) {
	return d.QueryContext(context.Background(), q, args...)
}
func (d *DB) QueryRow(q string, args ...any) *sql.Row {
	return d.QueryRowContext(context.Background(), q, args...)
}
func (d *DB) Close() error { return d.db.Close() }

func (t *Tx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, t.dialect.rebind(q), args...)
}
func (t *Tx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, t.dialect.rebind(q), args...)
}
func (t *Tx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.dialect.rebind(q), args...)
}
func (t *Tx) Exec(q string, args ...any) (sql.Result, error) {
	return t.ExecContext(context.Background(), q, args...)
}
func (t *Tx) Query(q string, args ...any) (*sql.Rows, error) {
	return t.QueryContext(context.Background(), q, args...)
}
func (t *Tx) QueryRow(q string, args ...any) *sql.Row {
	return t.QueryRowContext(context.Background(), q, args...)
}

// InTx runs fn inside a transaction, committing when it returns nil and
// rolling back otherwise.
//
// The rollback is the reason this exists. Every hand-rolled version of it in
// this repo was correct, and each one had to remember `defer tx.Rollback()`,
// remember that the deferred rollback is a no-op after Commit, and remember to
// return the commit error. The version that forgets one does not fail a test —
// it leaks a transaction on an error path nobody exercises, and with one
// connection per handle a leaked transaction is not a leak, it is a deadlock.
func (d *DB) InTx(ctx context.Context, fn func(*Tx) error) error {
	return d.inTx(ctx, nil, fn)
}

// InReadTx runs fn in a read-only repeatable-read transaction.
//
// For a read that was one statement and became several — a list chunked to
// stay under the parameter limit, say. One statement is a consistent snapshot
// by definition; several under PostgreSQL's default READ COMMITTED are not,
// and rows can appear or vanish between chunks. Callers that need the chunks
// to agree ask for this; callers that do not should say so and use InTx or no
// transaction at all, because the stricter level is not free.
func (d *DB) InReadTx(ctx context.Context, fn func(*Tx) error) error {
	return d.inTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead}, fn)
}

func (d *DB) inTx(ctx context.Context, opts *sql.TxOptions, fn func(*Tx) error) error {
	tx, err := d.db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("storage: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once Commit has succeeded
	if err := fn(&Tx{tx: tx, dialect: d.dialect}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}
