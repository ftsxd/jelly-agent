package task

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
)

// A task id is derived from the run that opened it, so it stays readable and
// stays joinable — every other table is keyed by (session, invocation), and an
// id that is those two spelled out can be traced by eye through any of them.
func TestIDRoundTrips(t *testing.T) {
	for _, tc := range []struct{ session, inv string }{
		{"web-1788515416639163000-1", "e-76faf8b2-1234"},
		{"schedule-nightly", "inv-1"},
	} {
		s, i := Split(ID(tc.session, tc.inv))
		if s != tc.session || i != tc.inv {
			t.Errorf("round trip of (%q,%q) gave (%q,%q)", tc.session, tc.inv, s, i)
		}
	}
	// A malformed id must not silently become something else.
	s, i := Split("no-slash")
	if s != "no-slash" || i != "" {
		t.Errorf("Split(%q) = (%q,%q)", "no-slash", s, i)
	}
}

func TestLinkingRunsToATask(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")

	// Nothing linked: every run is its own task, which is the common case and
	// the only case for anything written before this table existed.
	got, err := OfSession(db, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("an empty store reported links: %v", got)
	}

	if err := Link(db, "web-1/inv-1", "web-1", "inv-2"); err != nil {
		t.Fatal(err)
	}
	got, err = OfSession(db, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if got["inv-2"] != "web-1/inv-1" {
		t.Errorf("links = %v", got)
	}

	// Idempotent: a retry or a reconnect must not create a second membership.
	if err := Link(db, "web-1/inv-1", "web-1", "inv-2"); err != nil {
		t.Fatal(err)
	}
	got, _ = OfSession(db, "web-1")
	if len(got) != 1 {
		t.Errorf("re-linking created %d rows", len(got))
	}

	// Another session's links are not this one's.
	if other, _ := OfSession(db, "web-2"); len(other) != 0 {
		t.Errorf("cross-session leak: %v", other)
	}
}

func TestLinkRejectsIncompleteIdentities(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	for _, tc := range [][3]string{
		{"", "s", "i"}, {"t", "", "i"}, {"t", "s", ""},
	} {
		if err := Link(db, tc[0], tc[1], tc[2]); err == nil {
			t.Errorf("Link(%q,%q,%q) was accepted", tc[0], tc[1], tc[2])
		}
	}
}

// The override exists so a test or a second deployment can point elsewhere.
// A store that resolves its own path ignores it — which is how a test came to
// write links into the developer's real database.
func TestTheDatabasePathIsHonoured(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a.db")
	b := filepath.Join(t.TempDir(), "b.db")
	if err := Link(a, "web-1/inv-1", "web-1", "inv-2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := OfSession(b, "web-1"); len(got) != 0 {
		t.Errorf("a link written to one database was visible in another: %v", got)
	}
	if got, _ := OfSession(a, "web-1"); len(got) != 1 {
		t.Errorf("the link is not in the database it was written to")
	}
}

// Linking a run while another writer holds the file.
//
// This database is shared: the ADK session store, the delivery store and the
// metrics recorder all write to it during a turn, and Link runs in the middle
// of one. Without a busy timeout the call fails on a lock it would have got a
// few milliseconds later, and a failed Link is not loud — it silently splits a
// continuation off into a task of its own.
func TestLinkingWhileAnotherWriterHoldsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := Link(path, "s/inv-1", "s", "inv-1"); err != nil {
		t.Fatal(err) // create the file and the schema first
	}

	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)
	if _, err := other.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		t.Fatal(err)
	}

	// A write transaction held open across the Link, which is exactly what a
	// turn's other writers look like from here.
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS busy(x)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO busy VALUES(1)`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- Link(path, "s/inv-1", "s", "inv-2") }()

	// Long enough that a caller without a busy timeout has already given up.
	time.Sleep(200 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("另一个写入方持锁时 Link 失败: %v", err)
	}

	got, err := OfSession(path, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("links = %d, want 2: %v", len(got), got)
	}
}

// The file this store opens is shared with the session store, the delivery
// store and the metrics recorder, so it has to be opened the way they open it.
//
// Asserted on the mode rather than on a race, because the driver waits on a
// lock through unlock_notify whether or not a busy timeout is set — so a test
// that only holds a lock passes either way and proves nothing. The journal
// mode is a property of the file and can be read back.
func TestTheStoreOpensTheSharedFileInWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := Link(path, "s/inv-1", "s", "inv-1"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q，与共用同一个文件的其他存储不一致", mode)
	}
}

// A run may only join a task in its own conversation.
//
// The task id comes from the client, and it names the session that opened the
// task — so without this a run could claim membership of any task in any
// session, and the board would show that other session's task wearing this
// run's steps and title. Runs are folded one session at a time, so nothing
// downstream could have made sense of the result either.
func TestARunCannotJoinAnotherSessionsTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := Link(path, "web-b/inv-1", "web-a", "inv-9"); err == nil {
		t.Fatal("跨会话的任务归属被接受了")
	}
	if got, err := OfSession(path, "web-a"); err != nil || len(got) != 0 {
		t.Errorf("被拒绝的归属仍然写进了库: %v, %v", got, err)
	}
	// Its own session is fine, which is the whole normal path.
	if err := Link(path, "web-a/inv-1", "web-a", "inv-2"); err != nil {
		t.Fatalf("同会话的续跑被拒绝了: %v", err)
	}
	if got, _ := OfSession(path, "web-a"); got["inv-2"] != "web-a/inv-1" {
		t.Errorf("links = %v", got)
	}
}
