package metrics

import (
	"path/filepath"
	"testing"
	"time"
)

// The duration a run's tools took lives only here.
//
// The event stream deliberately carries no elapsed time: ADK merges parallel
// tool responses into one event that reuses the first one's timestamp, so a
// frame-to-frame delta is not a duration. Reporting it as one would be worse
// than reporting nothing — which is why the console has to come to this table
// for it, and why this query exists.
func TestByInvocationReturnsWhatARunActuallyDid(t *testing.T) {
	rec, err := NewRecorder(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })

	base := GatewayCall{SessionID: "s1", InvocationID: "inv1", Agent: "root", StartedAt: time.Now()}
	first := base
	first.CallID, first.Tool, first.Duration, first.OK = "c1", "get_metrics", 1200*time.Millisecond, true
	first.EvidenceID, first.Retrievable, first.ResultBytes = "e1", true, 4096
	if err := rec.RecordGatewayCall(first); err != nil {
		t.Fatal(err)
	}
	second := base
	second.CallID, second.Tool, second.Duration = "c2", "get_logs", 45*time.Second
	second.OK, second.ErrKind, second.Err = false, "timeout", "context deadline exceeded"
	if err := rec.RecordGatewayCall(second); err != nil {
		t.Fatal(err)
	}
	// A different run in the same session must not leak in — a task is one
	// invocation, so listing the session would show another question's work.
	other := base
	other.InvocationID, other.CallID, other.Tool, other.OK = "inv2", "c3", "get_pods", true
	if err := rec.RecordGatewayCall(other); err != nil {
		t.Fatal(err)
	}

	rows, err := rec.ByInvocation("s1", "inv1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
	}
	if rows[0].Tool != "get_metrics" || rows[0].DurationMS != 1200 {
		t.Errorf("first = %+v, want get_metrics at 1200ms", rows[0])
	}
	if !rows[0].OK || rows[0].EvidenceID != "e1" || !rows[0].Retrievable {
		t.Errorf("first = %+v", rows[0])
	}
	if rows[1].OK || rows[1].ErrKind != "timeout" || rows[1].DurationMS != 45000 {
		t.Errorf("second = %+v, want a 45s timeout", rows[1])
	}
	// Oldest first, so the console can lay them out in the order they happened.
	if !rows[0].At.Before(rows[1].At) && rows[0].CallID != "c1" {
		t.Errorf("order = %q, %q", rows[0].CallID, rows[1].CallID)
	}
}

// An empty invocation means "the whole session", for sessions that predate
// invocation ids being recorded.
func TestByInvocationWithoutARunReturnsTheSession(t *testing.T) {
	rec, err := NewRecorder(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	for _, inv := range []string{"inv1", "inv2"} {
		c := GatewayCall{SessionID: "s1", InvocationID: inv, CallID: inv, Tool: "t", OK: true, StartedAt: time.Now()}
		if err := rec.RecordGatewayCall(c); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := rec.ByInvocation("s1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("rows = %d, want the whole session", len(rows))
	}
}

func TestByInvocationIsSafeOnNothing(t *testing.T) {
	var rec *Recorder
	if rows, err := rec.ByInvocation("s1", "inv1"); err != nil || rows != nil {
		t.Errorf("nil recorder = %v, %v", rows, err)
	}
	var tr *Tracker
	if rows, err := tr.ByInvocation("s1", "inv1"); err != nil || rows != nil {
		t.Errorf("nil tracker = %v, %v", rows, err)
	}
}
