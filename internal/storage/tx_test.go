package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func scratch(t *testing.T) *sql.DB {
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

func rows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM seqs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInTxCommitsOnSuccess(t *testing.T) {
	db := scratch(t)
	err := InTx(context.Background(), db, func(tx *sql.Tx) error {
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
	err := InTx(context.Background(), db, func(tx *sql.Tx) error {
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
	err := InTx(context.Background(), db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO seqs VALUES ('a', 1)`); err != nil {
			return err
		}
		return tx.Rollback() // commit will now fail
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

func TestRetryOnConflictRetriesOnlyAConflict(t *testing.T) {
	// This loop cannot be exercised through a store: on SQLite one writer at a
	// time means the race it exists for never happens. Testing it as a
	// function is the only way it gets covered before the networked database
	// arrives — and by then it is load-bearing.
	db := scratch(t)
	if _, err := db.Exec(`INSERT INTO seqs VALUES ('a', 1)`); err != nil {
		t.Fatal(err)
	}
	conflict := func() error {
		_, err := db.Exec(`INSERT INTO seqs VALUES ('a', 1)`)
		return err
	}

	t.Run("succeeds on the attempt that stops conflicting", func(t *testing.T) {
		calls := 0
		got, err := RetryOnConflict(4, func() (int, error) {
			calls++
			if calls < 3 {
				return 0, conflict()
			}
			return 7, nil
		})
		if err != nil || got != 7 {
			t.Fatalf("got %d, %v", got, err)
		}
		if calls != 3 {
			t.Errorf("calls = %d, want 3", calls)
		}
	})

	t.Run("gives up after the bound", func(t *testing.T) {
		calls := 0
		_, err := RetryOnConflict(4, func() (int, error) {
			calls++
			return 0, conflict()
		})
		if err == nil {
			t.Fatal("a permanent conflict was reported as success")
		}
		if calls != 4 {
			t.Errorf("calls = %d, want 4 — the bound is not being honoured", calls)
		}
	})

	t.Run("does not retry anything else", func(t *testing.T) {
		calls := 0
		other := errors.New("connection refused")
		_, err := RetryOnConflict(4, func() (int, error) {
			calls++
			return 0, other
		})
		if !errors.Is(err, other) {
			t.Fatalf("err = %v", err)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 — retrying an error retrying cannot fix", calls)
		}
	})

	t.Run("runs once even when told zero", func(t *testing.T) {
		calls := 0
		if _, err := RetryOnConflict(0, func() (int, error) { calls++; return 1, nil }); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1", calls)
		}
	})
}
