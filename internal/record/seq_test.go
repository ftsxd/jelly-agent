package record

import (
	"context"
	"path/filepath"
	"testing"
)

// The contract for a handle, stated as a test because it changed.
//
// It used to be contiguous: e1, e2, e3, with the number computed as
// MAX(seq)+1 inside the insert. That read and wrote without holding anything,
// so N deliveries landing together all read the same maximum and the unique
// index rejected all but one — measured against PostgreSQL, the unluckiest of
// N concurrent writers needed exactly N attempts, and six parallel tools lost
// two results outright.
//
// A counter that blocks fixes that, and costs contiguity: re-delivering a call
// spends a number the row does not take, because the row keeps the handle the
// model was already shown. So the promise is now weaker and, unlike the old
// one, actually kept:
//
//	monotonic — a later delivery never has a lower number
//	never reused — a number names one delivery, for the life of the session
//	not contiguous — there may be gaps
func TestHandlesAreMonotonicAndNeverReused(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))

	first, err := s.Put(ctx, rec("s1", "c1", []byte("a")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Put(ctx, rec("s1", "c2", []byte("b")))
	if err != nil {
		t.Fatal(err)
	}
	if first != "e1" || second != "e2" {
		t.Fatalf("handles = %q, %q; want e1, e2", first, second)
	}

	// Re-delivering a call keeps its handle. Anything else would move a
	// reference the model has already been given.
	again, err := s.Put(ctx, rec("s1", "c1", []byte("a, longer")))
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("re-delivery of c1 returned %q, want %q", again, first)
	}
	// And the newer content is what comes back.
	got, err := s.ReadLabel(ctx, scope("s1"), first, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != "a, longer" {
		t.Errorf("payload = %q, want the re-delivered one", got.Data)
	}

	// The number that re-delivery spent is gone: the next handle skips it.
	// This is the gap, asserted rather than tolerated.
	third, err := s.Put(ctx, rec("s1", "c3", []byte("c")))
	if err != nil {
		t.Fatal(err)
	}
	if third != "e4" {
		t.Errorf("next handle = %q, want e4 — e3 was spent by the re-delivery", third)
	}
	// e3 names nothing, and says so rather than resolving to a neighbour.
	if _, err := s.ReadLabel(ctx, scope("s1"), "e3", 0, 100); err == nil {
		t.Error("a handle in the gap resolved to something")
	}
}

// A restart must not hand out a number that already names something.
//
// This is the failure an in-process counter has and a stored one does not, and
// it is the reason the number lives in the database at all.
func TestNumberingContinuesAcrossAReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	s := open(t, path)
	for _, call := range []string{"c1", "c2", "c3"} {
		if _, err := s.Put(ctx, rec("s1", call, []byte("x"))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := open(t, path)
	got, err := reopened.Put(ctx, rec("s1", "c4", []byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if got != "e4" {
		t.Errorf("after a reopen the next handle is %q, want e4", got)
	}
}

// Deleting a session takes its counter with it.
//
// Left behind, the row still names the session — so a purge meant to remove
// every trace has not — and a session id that comes back would be numbered
// from where the deleted conversation stopped, producing a handle that looks
// like history nobody can read.
func TestDeletingASessionRemovesItsCounter(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	for _, call := range []string{"c1", "c2", "c3"} {
		if _, err := s.Put(ctx, rec("s1", call, []byte("x"))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(ctx, scope("s1")); err != nil {
		t.Fatal(err)
	}

	if n := countSeqRows(t, s); n != 0 {
		t.Errorf("%d counter rows survived the delete — the session is still named", n)
	}
	// A session id that comes back starts over rather than continuing from a
	// conversation that no longer exists.
	got, err := s.Put(ctx, rec("s1", "c1", []byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if got != "e1" {
		t.Errorf("a reused session id got %q, want e1", got)
	}
}

func countSeqRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM tool_result_seq`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The counter is seeded from MAX(seq), and the difference from COUNT(*) only
// shows once there is a gap — which the new contract creates.
//
// A session whose numbers are 1, 2, 4 has three rows. Seeded by COUNT(*) the
// counter would say 3 and hand out 4 next, which the unique index already
// holds: the very next delivery in that session fails, on a database that
// opened cleanly. Seeded by MAX(seq) it says 4 and hands out 5.
func TestSeedingUsesTheHighestNumberNotTheCount(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	s := open(t, path)
	for _, call := range []string{"c1", "c2"} {
		if _, err := s.Put(ctx, rec("s1", call, []byte("x"))); err != nil {
			t.Fatal(err)
		}
	}
	// Spend e3 without storing it, the way a re-delivery does.
	if _, err := s.Put(ctx, rec("s1", "c1", []byte("again"))); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Put(ctx, rec("s1", "c4", []byte("x"))); err != nil || got != "e4" {
		t.Fatalf("setup: %q, %v — want e4 with e3 skipped", got, err)
	}

	// Now discard the counter, as a database migrated from before it existed
	// would have, and reopen so migrate seeds it again.
	if _, err := s.db.Exec(`DELETE FROM tool_result_seq`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := open(t, path)
	got, err := reopened.Put(ctx, rec("s1", "c5", []byte("x")))
	if err != nil {
		t.Fatalf("the first delivery after seeding failed: %v", err)
	}
	if got != "e5" {
		t.Errorf("next handle = %q, want e5 — rows are 1,2,4 so COUNT(*) would say e4", got)
	}
}
