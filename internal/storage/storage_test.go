package storage

import (
	"path/filepath"
	"testing"
)

func TestOpenCreatesTheParentDirectory(t *testing.T) {
	// Three of the six openers this replaced did their own MkdirAll and three
	// did not, so whether a fresh install worked depended on which store
	// happened to open the file first.
	path := filepath.Join(t.TempDir(), "nested", "deeper", "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (a TEXT)`); err != nil {
		t.Fatalf("the handle is not usable: %v", err)
	}
}

func TestOpenAppliesTheSharedSettings(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// WAL is the one that matters across handles: without it a reader on one
	// store's connection blocks a writer on another's, against the same file.
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var sync int
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != 1 { // NORMAL
		t.Errorf("synchronous = %d, want 1 (NORMAL)", sync)
	}
}

func TestOpenRejectsAnEmptyReference(t *testing.T) {
	// sqlite would happily open "" as a private temporary database, and a
	// store built on one loses everything at Close without ever erroring.
	if _, err := Open(""); err == nil {
		t.Fatal("an empty reference opened a database")
	}
}

func TestEnsureColumnsAddsOnlyWhatIsMissing(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE runs (id INTEGER, kept TEXT NOT NULL DEFAULT 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}

	cols := []Column{
		{"kept", "ALTER TABLE runs ADD COLUMN kept TEXT NOT NULL DEFAULT 'clobbered'"},
		{"added", "ALTER TABLE runs ADD COLUMN added TEXT NOT NULL DEFAULT 'new'"},
	}
	if err := EnsureColumns(db, "runs", cols); err != nil {
		t.Fatal(err)
	}

	// The existing column kept its value: the DDL for it must not have run.
	var kept, added string
	if err := db.QueryRow(`SELECT kept, added FROM runs WHERE id = 1`).Scan(&kept, &added); err != nil {
		t.Fatal(err)
	}
	if kept != "x" {
		t.Errorf("kept = %q — the ALTER ran on a column that was already there", kept)
	}
	if added != "new" {
		t.Errorf("added = %q, want the new column's default", added)
	}
}

func TestEnsureColumnsIsIdempotent(t *testing.T) {
	// It runs on every startup, so the second startup has to be a no-op.
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE runs (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	cols := []Column{{"note", "ALTER TABLE runs ADD COLUMN note TEXT NOT NULL DEFAULT ''"}}
	for i := range 3 {
		if err := EnsureColumns(db, "runs", cols); err != nil {
			t.Fatalf("startup %d: %v", i+1, err)
		}
	}
}

func TestEnsureColumnsReportsABadDDL(t *testing.T) {
	// A typo in a migration must not be swallowed as "already migrated".
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE runs (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	err = EnsureColumns(db, "runs", []Column{{"note", "ALTER TABLE runs ADD COLUMN note NOT A TYPE"}})
	if err == nil {
		t.Fatal("a malformed ALTER was reported as success")
	}
}
