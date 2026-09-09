package schedule

import (
	"database/sql"
	"github.com/jelly-agent/jelly-agent/internal/storage"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
)

// withHome points DefaultDBPath at a temporary directory, since the store
// resolves its own path rather than taking one.
func withHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// scratch is this package's store, on a database of the test's own.
//
// It used to be enough to set HOME, because the store resolved its own path —
// which is exactly the bug that made it write a deployment's schedule history
// into the default database instead of the configured one. Now it takes a
// handle, so a test hands it one.
func scratch(t *testing.T) *storage.DB {
	t.Helper()
	return stateDB(t, filepath.Join(t.TempDir(), "state.db"))
}

func TestRunCarriesTheIdsThatJoinItToItsSteps(t *testing.T) {
	withHome(t)
	db := scratch(t)
	started := time.Now().Add(-time.Minute)
	if err := Record(db, "nightly", started, "succeeded", "done", "", "schedule-nightly", "inv-abc"); err != nil {
		t.Fatal(err)
	}
	runs, total, err := List(db, "nightly", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(runs) != 1 {
		t.Fatalf("total=%d rows=%d", total, len(runs))
	}
	// Without these two a scheduled run is a status and a blob of text, with
	// no way to reach the steps, tool calls or stored results behind it —
	// every other table is keyed by exactly this pair.
	if runs[0].SessionID != "schedule-nightly" || runs[0].InvocationID != "inv-abc" {
		t.Errorf("session=%q invocation=%q; the run cannot be joined to anything",
			runs[0].SessionID, runs[0].InvocationID)
	}
}

// A run that failed before the agent started has no ids, and empty is the
// honest answer — better than a fabricated one that resolves to nothing.
func TestARunThatNeverStartedRecordsNoIds(t *testing.T) {
	withHome(t)
	db := scratch(t)
	if err := Record(db, "nightly", time.Now(), "failed", "", "build agent: no provider", "", ""); err != nil {
		t.Fatal(err)
	}
	runs, _, err := List(db, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].SessionID != "" || runs[0].InvocationID != "" {
		t.Errorf("rows = %+v", runs)
	}
}

// The trap this file used to sit in: CREATE TABLE IF NOT EXISTS does nothing
// to a table that already exists, and this store ran it inline on every query
// — so a deployment that had ever run the old binary would keep the old shape
// forever and every read would fail on the missing columns.
func TestOpeningAPreIdsDatabase(t *testing.T) {
	// Written by hand, then opened through this package — so the assertions
	// below are about the migration and not about a fresh table.
	//
	// It used to have to place the file at $HOME/.jelly-agent/state.db,
	// because the store resolved its own path. Now it takes a handle, so the
	// test simply names one.
	path := filepath.Join(t.TempDir(), "state.db")

	// Exactly the old shape.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schedule_runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT, task TEXT NOT NULL,
		started_at DATETIME NOT NULL, finished_at DATETIME NOT NULL,
		status TEXT NOT NULL, output TEXT, error TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO schedule_runs(task,started_at,finished_at,status,output,error)
		VALUES('old', ?, ?, 'succeeded', 'legacy output', '')`, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db := stateDB(t, path) // runs EnsureSchema, which is where migrate lives

	// The old row survives, readable, with empty ids.
	runs, total, err := List(db, "", 10, 0)
	if err != nil {
		t.Fatalf("a pre-ids database became unreadable: %v", err)
	}
	if total != 1 || runs[0].Output != "legacy output" {
		t.Fatalf("rows = %+v", runs)
	}
	if runs[0].SessionID != "" {
		t.Errorf("session id = %q, want empty on a legacy row", runs[0].SessionID)
	}
	// And new rows can use the new columns.
	if err := Record(db, "new", time.Now(), "succeeded", "", "", "s1", "i1"); err != nil {
		t.Fatalf("writing after the migration: %v", err)
	}
}
