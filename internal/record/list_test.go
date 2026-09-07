package record

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seed(t *testing.T, s *Store, session, invocation, call, tool string, size int) string {
	t.Helper()
	ref, err := s.Put(t.Context(), Record{
		Scope: scope(session), InvocationID: invocation, CallID: call,
		Tool: tool, At: time.Now(), Payload: []byte(strings.Repeat("x", size)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// A listing is for deciding what to open, so it must not open anything.
//
// A single row can be megabytes; loading payloads to build a list is the same
// mistake the delivery budget exists to prevent, made on the other side of the
// wire. The Item type has no payload field at all, which is the point — this
// asserts the sizes are still reported so the console can show scale without
// reading scale.
func TestListReportsScaleWithoutReadingIt(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	seed(t, s, "s1", "inv1", "c1", "get_logs", 40000)
	seed(t, s, "s1", "inv1", "c2", "list_alert_rules", 300)

	items, err := s.List(t.Context(), scope("s1"), ListOpts{InvocationID: "inv1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Label != "e1" || items[1].Label != "e2" {
		t.Errorf("labels = %q, %q; want the handles the model was shown", items[0].Label, items[1].Label)
	}
	if items[0].Bytes != 40000 || items[1].Bytes != 300 {
		t.Errorf("sizes = %d, %d", items[0].Bytes, items[1].Bytes)
	}
	if items[0].Tool != "get_logs" || items[0].CallID != "c1" {
		t.Errorf("item = %+v", items[0])
	}
	if items[0].SHA256 == "" {
		t.Error("no digest, so nothing can be checked against what was stored")
	}
	if items[0].At.IsZero() {
		t.Error("no timestamp")
	}
}

// One run's artifacts, not the whole session's. A task is one invocation, so
// listing the session would show a chat's other questions under this one.
func TestListNarrowsToOneRun(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	seed(t, s, "s1", "inv1", "c1", "get_logs", 10)
	seed(t, s, "s1", "inv2", "c2", "get_metrics", 10)
	seed(t, s, "s1", "inv2", "c3", "get_metrics", 10)

	one, err := s.List(t.Context(), scope("s1"), ListOpts{InvocationID: "inv2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 2 {
		t.Fatalf("run inv2 has %d artifacts, want 2", len(one))
	}
	for _, it := range one {
		if it.InvocationID != "inv2" {
			t.Errorf("artifact from %s leaked into inv2's list", it.InvocationID)
		}
	}

	all, err := s.List(t.Context(), scope("s1"), ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("whole session = %d, want 3", len(all))
	}
}

// An expired artifact stays in the list, marked. Dropping it would make a run
// look like it produced less than it did, and the handle is still the thing a
// stored conclusion cites.
func TestExpiredArtifactsStayInTheListAndSaySo(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	old, err := s.Put(t.Context(), Record{
		Scope: scope("s1"), InvocationID: "inv1", CallID: "c1",
		Tool: "get_logs", At: time.Now().Add(-30 * 24 * time.Hour),
		Payload: []byte("ancient"),
	})
	if err != nil {
		t.Fatal(err)
	}
	seed(t, s, "s1", "inv1", "c2", "get_logs", 100)
	if n, err := s.Sweep(t.Context(), 7*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}

	items, err := s.List(t.Context(), scope("s1"), ListOpts{InvocationID: "inv1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("an expired artifact vanished from the listing: %d items", len(items))
	}
	var expired *Item
	for i := range items {
		if items[i].Label == old {
			expired = &items[i]
		}
	}
	if expired == nil {
		t.Fatalf("handle %s is not in the listing", old)
	}
	if !expired.Expired || expired.ExpiredAt.IsZero() {
		t.Errorf("expired artifact = %+v; the console cannot tell it apart from a live one", expired)
	}
	// The original size survives, so the console can still say how much was lost.
	if expired.Bytes != len("ancient") {
		t.Errorf("bytes = %d, want %d", expired.Bytes, len("ancient"))
	}
}

// Scope is the permission boundary here as everywhere else.
func TestListDoesNotCrossSessions(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	seed(t, s, "s1", "inv1", "c1", "get_logs", 10)
	items, err := s.List(t.Context(), scope("s2"), ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("another session's artifacts are visible: %+v", items)
	}
}

func TestListBoundsItsPage(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	for i := 0; i < 5; i++ {
		seed(t, s, "s1", "inv1", string(rune('a'+i)), "get_logs", 10)
	}
	items, err := s.List(t.Context(), scope("s1"), ListOpts{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Label != "e2" {
		t.Errorf("page = %d items starting at %v", len(items), items[0].Label)
	}
	if got, _ := s.List(t.Context(), scope("s1"), ListOpts{Limit: 1 << 20}); len(got) != 5 {
		t.Errorf("an absurd limit returned %d", len(got))
	}
}

// Which runs left something behind, for a page of sessions at once.
//
// The task list asks this of every session it reads back, and it reads back
// hundreds. One query per session made it the dominant cost of that page for
// an answer that is a single distinct-select over an index.
func TestRecordedRunsAnswersForManySessionsAtOnce(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	put := func(session, inv, call string) {
		t.Helper()
		r := rec(session, call, []byte("x"))
		r.InvocationID = inv
		if _, err := s.Put(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	put("s1", "inv-1", "c1")
	put("s1", "inv-1", "c2") // the same run twice: one entry, not two
	put("s1", "inv-2", "c1")
	put("s2", "inv-1", "c1")

	got, err := s.RecordedRuns(t.Context(), "jelly", "u", []string{"s1", "s2", "s3", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["s1"]) != 2 || !got["s1"]["inv-1"] || !got["s1"]["inv-2"] {
		t.Errorf("s1 = %v, want both runs", got["s1"])
	}
	if len(got["s2"]) != 1 || !got["s2"]["inv-1"] {
		t.Errorf("s2 = %v", got["s2"])
	}
	// A session that stored nothing is absent rather than empty-and-present:
	// the caller reads a missing entry as "nothing here".
	if _, present := got["s3"]; present {
		t.Errorf("s3 存了个空的进来: %v", got["s3"])
	}
	// Another scope's rows are not this scope's answer.
	other, err := s.RecordedRuns(t.Context(), "jelly", "someone-else", []string{"s1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("越过了 scope: %v", other)
	}
}
