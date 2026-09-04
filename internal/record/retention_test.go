package record

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func atTime(t *testing.T, s *Store, session, call string, when time.Time, payload string) string {
	t.Helper()
	ref, err := s.Put(t.Context(), Record{
		Scope: scope(session), InvocationID: "inv1", CallID: call,
		Tool: "get_logs", At: when, Payload: []byte(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// The distinction the whole design rests on: an expired handle must not come
// back as "not found". Not-found means look again — a typo, the wrong session,
// a call that never landed. Expired means the bytes are gone and the only way
// to get them is to run the tool again. Collapsing the two sends the model
// hunting for a reference that no longer exists.
func TestExpiredHandleIsNotConfusedWithAMissingOne(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	old := atTime(t, s, "s1", "c-old", time.Now().Add(-30*24*time.Hour), "ancient logs")
	fresh := atTime(t, s, "s1", "c-new", time.Now(), "today's logs")

	n, err := s.Sweep(t.Context(), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expired %d rows, want 1 (only the 30-day-old one)", n)
	}

	// The old handle: expired, and it says so.
	_, err = s.ReadLabel(t.Context(), scope("s1"), old, 0, DefaultWindow)
	if !errors.Is(err, ErrExpired) {
		t.Errorf("read of an expired handle: %v, want ErrExpired", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("an expired handle also reported not-found; the model would look for it again")
	}
	// Search draws the same distinction. A search over a dropped payload
	// returning zero matches would be read as evidence rather than absence.
	_, err = s.Search(t.Context(), scope("s1"), old, "ancient", SearchOpts{})
	if !errors.Is(err, ErrExpired) {
		t.Errorf("search of an expired handle: %v, want ErrExpired", err)
	}

	// A handle that was never issued stays not-found.
	_, err = s.ReadLabel(t.Context(), scope("s1"), "e99", 0, DefaultWindow)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("read of an unissued handle: %v, want ErrNotFound", err)
	}

	// And the fresh one is untouched.
	chunk, err := s.ReadLabel(t.Context(), scope("s1"), fresh, 0, DefaultWindow)
	if err != nil {
		t.Fatalf("the fresh delivery was swept: %v", err)
	}
	if string(chunk.Data) != "today's logs" {
		t.Errorf("fresh payload = %q", chunk.Data)
	}
}

// The error has to name what was lost, or an operator reading the log cannot
// tell a retention problem from a bug.
func TestExpiryErrorSaysWhatItWas(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	ref := atTime(t, s, "s1", "c1", time.Now().Add(-30*24*time.Hour), strings.Repeat("x", 1234))
	if _, err := s.Sweep(t.Context(), time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err := s.ReadLabel(t.Context(), scope("s1"), ref, 0, DefaultWindow)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{ref, "1234"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

// Sweeping twice must not double-count, because the count goes into a log line
// an operator reads as "how much is aging out".
func TestSweepIsIdempotent(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	atTime(t, s, "s1", "c1", time.Now().Add(-30*24*time.Hour), "old")

	first, err := s.Sweep(t.Context(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Sweep(t.Context(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != 0 {
		t.Errorf("sweeps expired %d then %d, want 1 then 0", first, second)
	}
}

// Retention off is a supported choice, not an oversight: an operator who wants
// an archive should not have to pick a number large enough to never fire.
func TestNonPositiveRetentionKeepsEverything(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	ref := atTime(t, s, "s1", "c1", time.Now().Add(-365*24*time.Hour), "very old")

	n, err := s.Sweep(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expired %d rows with retention off", n)
	}
	if _, err := s.ReadLabel(t.Context(), scope("s1"), ref, 0, DefaultWindow); err != nil {
		t.Errorf("a year-old delivery was unreadable with retention off: %v", err)
	}
}

// A database written before expired_at existed must open and keep working.
// This is the trap that already cost a working store once: CREATE TABLE IF NOT
// EXISTS does nothing to a table that is already there, so the column has to
// be added explicitly or every read fails on a machine that had been fine.
func TestOpeningAPreExpiryDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	before := open(t, path)
	ref := atTime(t, before, "s1", "c1", time.Now(), "kept across the migration")
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	// Drop the column the way an older binary would have left it absent.
	raw := open(t, path)
	if _, err := raw.db.Exec(`ALTER TABLE tool_results DROP COLUMN expired_at`); err != nil {
		t.Skipf("this SQLite build cannot drop columns: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	after := open(t, path)
	chunk, err := after.ReadLabel(t.Context(), scope("s1"), ref, 0, DefaultWindow)
	if err != nil {
		t.Fatalf("a pre-expiry database became unreadable: %v", err)
	}
	if string(chunk.Data) != "kept across the migration" {
		t.Errorf("payload = %q", chunk.Data)
	}
	if _, err := after.Sweep(t.Context(), time.Hour); err != nil {
		t.Errorf("sweep on a migrated database: %v", err)
	}
}
