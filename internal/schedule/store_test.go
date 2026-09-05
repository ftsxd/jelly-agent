package schedule

import (
	"database/sql"
	"os"
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

func TestRunCarriesTheIdsThatJoinItToItsSteps(t *testing.T) {
	withHome(t)
	started := time.Now().Add(-time.Minute)
	if err := Record("nightly", started, "succeeded", "done", "", "schedule-nightly", "inv-abc"); err != nil {
		t.Fatal(err)
	}
	runs, total, err := List("nightly", 10, 0)
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
	if err := Record("nightly", time.Now(), "failed", "", "build agent: no provider", "", ""); err != nil {
		t.Fatal(err)
	}
	runs, _, err := List("", 10, 0)
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
	withHome(t)
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".jelly-agent", "state.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	// Exactly the old shape, written by hand.
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

	// The old row survives, readable, with empty ids.
	runs, total, err := List("", 10, 0)
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
	if err := Record("new", time.Now(), "succeeded", "", "", "s1", "i1"); err != nil {
		t.Fatalf("writing after the migration: %v", err)
	}
}
