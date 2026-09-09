package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func scratch(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE seqs (owner TEXT NOT NULL, n INTEGER NOT NULL,
		UNIQUE (owner, n))`); err != nil {
		t.Fatal(err)
	}
	return db
}

func rows(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM seqs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInTxCommitsOnSuccess(t *testing.T) {
	db := scratch(t)
	err := db.InTx(context.Background(), func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO seqs VALUES ('a', 1)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := rows(t, db); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

func TestInTxRollsBackEveryStatementOnFailure(t *testing.T) {
	// The point of wrapping Put: the read-back and the write are one unit, so
	// a failure in the second must not leave the first behind.
	db := scratch(t)
	boom := errors.New("boom")
	err := db.InTx(context.Background(), func(tx *Tx) error {
		if _, err := tx.Exec(`INSERT INTO seqs VALUES ('a', 1)`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO seqs VALUES ('a', 2)`); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if n := rows(t, db); n != 0 {
		t.Errorf("rows = %d after rollback, want 0 — the first insert survived", n)
	}
}

func TestInTxReportsACommitFailure(t *testing.T) {
	// A fn that returns nil is not the same as a transaction that landed.
	db := scratch(t)
	err := db.InTx(context.Background(), func(tx *Tx) error {
		if _, err := tx.Exec(`INSERT INTO seqs VALUES ('a', 1)`); err != nil {
			return err
		}
		return tx.tx.Rollback() // commit will now fail
	})
	if err == nil {
		t.Fatal("a transaction that could not commit was reported as success")
	}
}

func TestIsUniqueViolationSeesTheCodeNotTheMessage(t *testing.T) {
	db := scratch(t)
	if _, err := db.Exec(`INSERT INTO seqs VALUES ('a', 1)`); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(`INSERT INTO seqs VALUES ('a', 1)`)
	if err == nil {
		t.Fatal("the unique index did not reject a duplicate")
	}
	if !IsUniqueViolation(err) {
		t.Errorf("a duplicate row was not classified as a uniqueness clash: %v", err)
	}
	// Wrapped the way callers wrap it, because errors.As has to reach through.
	if !IsUniqueViolation(fmt.Errorf("record: put s/c: %w", err)) {
		t.Error("wrapping the error hid the classification")
	}

	_, other := db.Exec(`INSERT INTO nosuchtable VALUES (1)`)
	if IsUniqueViolation(other) {
		t.Errorf("a missing table was classified as a uniqueness clash: %v", other)
	}
	if IsUniqueViolation(nil) {
		t.Error("nil was classified as a uniqueness clash")
	}
}
