package record

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func scope(session string) Scope {
	return Scope{AppName: "jelly", UserID: "u", SessionID: session}
}

func rec(session, call string, payload []byte) Record {
	return Record{
		Scope: scope(session), InvocationID: "inv1", CallID: call,
		Tool: "list_alert_rules", At: time.Now(),
		Payload: payload,
	}
}

// The acceptance loop: an oversized delivery is stored, the process goes away,
// and the part the prompt never saw is still readable afterwards.
//
// Reopening the file is the whole point — anything that only worked while the
// original handle was alive would not survive a restart, which is exactly when
// someone goes looking for what was cut.
func TestDeliverySurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	big := []byte(strings.Repeat("x", 40_000) + "TAIL")

	first := open(t, path)
	if _, err := first.Put(t.Context(), rec("s1", "c1", big)); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// A different Store over the same file, as a restarted process would have.
	again := open(t, path)
	var got []byte
	for offset := 0; ; {
		c, err := again.Read(t.Context(), scope("s1"), "c1", offset, 8192)
		if err != nil {
			t.Fatal(err)
		}
		if c.Total != len(big) {
			t.Fatalf("total = %d, want %d", c.Total, len(big))
		}
		got = append(got, c.Data...)
		offset = c.Offset + len(c.Data)
		if offset >= c.Total {
			break
		}
	}
	if !bytes.Equal(got, big) {
		t.Errorf("recovered %d bytes, want %d", len(got), len(big))
	}
	if !bytes.HasSuffix(got, []byte("TAIL")) {
		t.Error("the tail — the part a prompt would have cut — did not come back")
	}
}

// A window is a window. Storing everything is not a licence to return
// everything: the payload that was too large for a prompt is still too large
// for one response.
func TestReadIsWindowedAndBounded(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.Put(t.Context(), rec("s1", "c1", []byte(strings.Repeat("y", 100)))); err != nil {
		t.Fatal(err)
	}
	c, err := s.Read(t.Context(), scope("s1"), "c1", 10, 20)
	if err != nil {
		t.Fatal(err)
	}
	if c.Offset != 10 || len(c.Data) != 20 {
		t.Errorf("offset=%d len=%d, want 10 and 20", c.Offset, len(c.Data))
	}
	if c.Total != 100 {
		t.Errorf("total = %d, want the full length 100", c.Total)
	}
	// A limit above the ceiling is clamped rather than honoured.
	c, err = s.Read(t.Context(), scope("s1"), "c1", 0, MaxWindow*10)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Data) > MaxWindow {
		t.Errorf("returned %d bytes, over the %d ceiling", len(c.Data), MaxWindow)
	}
}

// Scope is the permission boundary, and it is enforced in the query rather
// than after it: a call id from one conversation must not reach another's
// payload.
func TestAnotherSessionCannotRead(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.Put(t.Context(), rec("s1", "c1", []byte("secret"))); err != nil {
		t.Fatal(err)
	}
	for _, sc := range []Scope{
		scope("s2"),
		{AppName: "jelly", UserID: "other", SessionID: "s1"},
		{AppName: "other", UserID: "u", SessionID: "s1"},
	} {
		if _, err := s.Read(t.Context(), sc, "c1", 0, 100); !errors.Is(err, ErrNotFound) {
			t.Errorf("scope %+v got err %v, want ErrNotFound", sc, err)
		}
	}
}

// A missing record and a record in someone else's scope answer the same way.
// Distinguishing them would tell a caller which sessions exist.
func TestUnknownCallIsNotFound(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.Read(t.Context(), scope("s1"), "nope", 0, 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// What the tool said about truncating its own output travels with the record,
// and silence is recorded as silence.
func TestUpstreamTruncationIsThreeState(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	for _, tc := range []struct {
		call string
		set  Upstream
		want Upstream
	}{
		{"c1", UpstreamYes, UpstreamYes},
		{"c2", UpstreamNo, UpstreamNo},
		{"c3", "", UpstreamUnknown}, // unset must not become "no"
	} {
		r := rec("s1", tc.call, []byte("x"))
		r.Upstream = tc.set
		if _, err := s.Put(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		c, err := s.Read(t.Context(), scope("s1"), tc.call, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if c.Upstream != tc.want {
			t.Errorf("%s: upstream = %q, want %q", tc.call, c.Upstream, tc.want)
		}
	}
}

// The checksum lets a caller reassembling several windows tell whether they
// belong to one payload.
func TestChecksumCoversTheWholePayload(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.Put(t.Context(), rec("s1", "c1", []byte("hello"))); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Read(t.Context(), scope("s1"), "c1", 0, 2)
	b, _ := s.Read(t.Context(), scope("s1"), "c1", 2, 3)
	if a.SHA256 == "" || a.SHA256 != b.SHA256 {
		t.Errorf("checksums differ across windows: %q vs %q", a.SHA256, b.SHA256)
	}
}

// An approved tool re-executes under the same call id, and the second delivery
// is the real one.
func TestRestoringTheSameCallReplacesIt(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.Put(t.Context(), rec("s1", "c1", []byte("placeholder"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(t.Context(), rec("s1", "c1", []byte("the real result"))); err != nil {
		t.Fatal(err)
	}
	c, err := s.Read(t.Context(), scope("s1"), "c1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Data) != "the real result" {
		t.Errorf("got %q, want the later delivery", c.Data)
	}
}

// A record with no session or no call id has no addressable identity, so it
// must be refused rather than written somewhere unreachable.
func TestPutRefusesAnUnaddressableRecord(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	for _, r := range []Record{
		{Scope: scope(""), CallID: "c1"},
		{Scope: scope("s1"), CallID: ""},
	} {
		if _, err := s.Put(t.Context(), r); err == nil {
			t.Errorf("Put(%+v) succeeded; it cannot be read back", r.Scope)
		}
	}
}

func TestDeleteRemovesASession(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.Put(t.Context(), rec("s1", "c1", []byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), scope("s1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(t.Context(), scope("s1"), "c1", 0, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound after delete", err)
	}
}

// The handle is assigned by the store, and that is what makes it survive a
// restart. An in-process counter would reset and hand the next turn a label
// that already refers to something else — the one thing a durable reference
// cannot do.
func TestHandlesAreUniquePerSessionAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	first := open(t, path)
	a, err := first.Put(t.Context(), rec("s1", "c1", []byte("first turn")))
	if err != nil {
		t.Fatal(err)
	}
	if a != "e1" {
		t.Fatalf("first handle = %q, want e1", a)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// A restarted process: nothing in memory, everything from the file.
	again := open(t, path)
	b, err := again.Put(t.Context(), rec("s1", "c2", []byte("after restart")))
	if err != nil {
		t.Fatal(err)
	}
	if b == a {
		t.Fatalf("both deliveries got %q; the counter reset across the restart", a)
	}
	if b != "e2" {
		t.Errorf("handle after restart = %q, want e2", b)
	}

	// And the old handle still resolves to the old payload.
	c, err := again.ReadLabel(t.Context(), scope("s1"), a, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Data) != "first turn" {
		t.Errorf("%s resolved to %q, want the first turn's payload", a, c.Data)
	}
}

// Numbering is per session, so two conversations both start at e1 without
// either being able to read the other's.
func TestHandlesAreNumberedPerSession(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	a, _ := s.Put(t.Context(), rec("s1", "c1", []byte("one")))
	b, _ := s.Put(t.Context(), rec("s2", "c1", []byte("two")))
	if a != "e1" || b != "e1" {
		t.Fatalf("handles = %q, %q; want each session to start at e1", a, b)
	}
	c, err := s.ReadLabel(t.Context(), scope("s2"), "e1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Data) != "two" {
		t.Errorf("s2's e1 resolved to %q", c.Data)
	}
	// And s1's e1 is not reachable from s2's scope by any handle.
	if _, err := s.ReadLabel(t.Context(), scope("s2"), "e2", 0, 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound — s2 has no e2", err)
	}
}

// Re-executing an approved tool writes the real result under the same call id.
// The handle must not move: anything already citing it has to keep resolving.
func TestHandleSurvivesAReplacedPayload(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	first, err := s.Put(t.Context(), rec("s1", "c1", []byte("placeholder")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Put(t.Context(), rec("s1", "c1", []byte("the real result")))
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("handle moved from %q to %q on re-store", first, second)
	}
	c, err := s.ReadLabel(t.Context(), scope("s1"), first, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Data) != "the real result" {
		t.Errorf("%s resolved to %q, want the later delivery", first, c.Data)
	}
}

// A handle from another conversation must not resolve, and a malformed one
// must miss rather than land on row zero.
func TestBadHandlesMiss(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.Put(t.Context(), rec("s1", "c1", []byte("secret"))); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"", "e", "e0", "e-1", "x1", "e1x", "1"} {
		if _, err := s.ReadLabel(t.Context(), scope("s1"), label, 0, 100); !errors.Is(err, ErrNotFound) {
			t.Errorf("label %q got err %v, want ErrNotFound", label, err)
		}
	}
	if _, err := s.ReadLabel(t.Context(), scope("s2"), "e1", 0, 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("another session resolved e1: %v", err)
	}
}

// Concurrent deliveries in one session must not share a handle. The unique
// index turns a race into a failed write rather than two results answering to
// the same name.
func TestConcurrentPutsGetDistinctHandles(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	const n = 20
	type res struct {
		label string
		err   error
	}
	out := make(chan res, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			l, err := s.Put(t.Context(), rec("s1", "c"+strconv.Itoa(i), []byte("x")))
			out <- res{l, err}
		}(i)
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		r := <-out
		if r.err != nil {
			// A losing racer is acceptable; a duplicate handle is not.
			continue
		}
		if seen[r.label] {
			t.Fatalf("handle %q was handed out twice", r.label)
		}
		seen[r.label] = true
	}
	if len(seen) == 0 {
		t.Fatal("no delivery was stored at all")
	}
}

// Opening a database written by the previous version must work.
//
// CREATE TABLE IF NOT EXISTS does nothing to a table that already exists, so
// the seq column and the index that depends on it have to be added
// explicitly. Without that, a machine that had been running fine fails at the
// first tool call after an upgrade — which is exactly what happened.
func TestOpeningAPreSeqDatabaseMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	// The previous shape: a label column, no seq, no unique index.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE tool_results (
			app_name TEXT NOT NULL, user_id TEXT NOT NULL, session_id TEXT NOT NULL,
			invocation_id TEXT NOT NULL, call_id TEXT NOT NULL,
			label TEXT NOT NULL DEFAULT '', tool TEXT NOT NULL,
			server TEXT NOT NULL DEFAULT '', at TEXT NOT NULL,
			bytes INTEGER NOT NULL, sha256 TEXT NOT NULL,
			upstream TEXT NOT NULL DEFAULT 'unknown', payload BLOB NOT NULL,
			PRIMARY KEY (app_name,user_id,session_id,invocation_id,call_id)
		)`); err != nil {
		t.Fatal(err)
	}
	// Two old rows in one session: both would land on seq 0 and collide with
	// the unique index unless they are numbered.
	for i, call := range []string{"old_a", "old_b"} {
		if _, err := db.Exec(`INSERT INTO tool_results
			(app_name,user_id,session_id,invocation_id,call_id,tool,at,bytes,sha256,payload)
			VALUES ('jelly','u','s1','inv0',?,'old_tool',?,1,'','x')`,
			call, time.Now().Add(time.Duration(i)*time.Second).UTC().Format(time.RFC3339Nano),
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := open(t, path)

	// The old rows got handles, distinct ones.
	a, err := s.Read(t.Context(), scope("s1"), "old_a", 0, 10)
	if err != nil {
		t.Fatalf("pre-existing row unreadable after migration: %v", err)
	}
	b, err := s.Read(t.Context(), scope("s1"), "old_b", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if a.Label == b.Label {
		t.Errorf("both migrated rows got handle %q", a.Label)
	}

	// And a new delivery continues the numbering rather than colliding.
	fresh, err := s.Put(t.Context(), rec("s1", "new_c", []byte("new")))
	if err != nil {
		t.Fatalf("put after migration: %v", err)
	}
	if fresh == a.Label || fresh == b.Label {
		t.Errorf("new handle %q collides with a migrated one", fresh)
	}
}

// Label and parseLabel are inverses, and a malformed handle must resolve to
// nothing rather than to row zero.
//
// Tested directly because the store happens to make the difference invisible:
// numbering starts at one, so a handle that parsed to zero would miss anyway.
// That is luck, not a guarantee — a backfill or an import that produced a zero
// row would turn "e0" into a way to read something nobody addressed.
func TestLabelRoundTripAndRejection(t *testing.T) {
	for _, seq := range []int{1, 2, 9, 10, 12345} {
		l := Label(seq)
		got, ok := parseLabel(l)
		if !ok || got != seq {
			t.Errorf("parseLabel(Label(%d)) = %d, %v", seq, got, ok)
		}
	}
	for _, bad := range []string{"", "e", "e0", "e-1", "e+1", "x1", "e1x", "1", "E1", "e 1", "e1.0"} {
		if n, ok := parseLabel(bad); ok {
			t.Errorf("parseLabel(%q) accepted, giving %d", bad, n)
		}
	}
}
