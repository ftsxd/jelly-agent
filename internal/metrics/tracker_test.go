package metrics

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestTracker(t *testing.T) (*Tracker, *Recorder) {
	t.Helper()
	rec, err := NewRecorder(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { rec.Close() })
	return NewTracker(rec), rec
}

func TestFinishPairsWithStartAndTimesTheCall(t *testing.T) {
	tr, rec := newTestTracker(t)

	tr.Start("call-1", "k8s_get_pods", map[string]any{"ns": "payment"})
	if got := tr.Pending(); got != 1 {
		t.Fatalf("Pending = %d, want 1", got)
	}
	time.Sleep(2 * time.Millisecond)

	row := tr.Finish(CallMeta{CallID: "call-1", Tool: "k8s_get_pods", SessionID: "s1"},
		map[string]any{"pods": "3/6 CrashLoopBackOff"}, nil)

	if !row.OK {
		t.Errorf("OK = false, want true")
	}
	if row.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", row.Duration)
	}
	if row.Args["ns"] != "payment" {
		t.Errorf("Args = %v, want the args recorded at Start", row.Args)
	}
	if row.ResultBytes == 0 {
		t.Errorf("ResultBytes = 0, want the encoded payload size")
	}
	if got := tr.Pending(); got != 0 {
		t.Errorf("Pending = %d after Finish, want 0", got)
	}

	var n int
	if err := rec.db.QueryRow(`SELECT count(*) FROM tool_calls WHERE ok = 1`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Errorf("rows with ok=1 = %d, want 1", n)
	}
}

// A tool that reports its failure inside an otherwise-successful response is
// still a failure. Missing this is how a success rate ends up reading 100%.
func TestFinishTreatsErrorInPayloadAsFailure(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.Start("call-2", "mysql_health", nil)

	row := tr.Finish(CallMeta{CallID: "call-2", Tool: "mysql_health"},
		map[string]any{"error": "dial tcp 10.0.0.1:3306: connection refused"}, nil)

	if row.OK {
		t.Fatal("OK = true, want false for a payload carrying an error")
	}
	if row.ErrKind != ErrUpstream {
		t.Errorf("ErrKind = %q, want %q", row.ErrKind, ErrUpstream)
	}
}

func TestFinishWithoutStartStillRecords(t *testing.T) {
	tr, _ := newTestTracker(t)
	// No Start: the process restarted mid-call, or the hook was added later.
	row := tr.Finish(CallMeta{CallID: "orphan", Tool: "web_search"}, nil, errors.New("boom"))
	if row.OK {
		t.Error("OK = true, want false")
	}
	if row.Duration != 0 {
		t.Errorf("Duration = %v, want 0 when the start was never seen", row.Duration)
	}
}

func TestNilTrackerAndRecorderAreSafe(t *testing.T) {
	var tr *Tracker
	tr.Start("x", "y", nil) // must not panic
	if got := tr.Pending(); got != 0 {
		t.Errorf("Pending = %d, want 0", got)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	// A tracker whose store failed to open still times and discards.
	degraded := NewTracker(nil)
	degraded.Start("a", "t", nil)
	if row := degraded.Finish(CallMeta{CallID: "a", Tool: "t"}, nil, nil); !row.OK {
		t.Error("degraded tracker reported failure for a successful call")
	}
}

func TestPendingIsBounded(t *testing.T) {
	tr, _ := newTestTracker(t)
	for i := 0; i < maxPending+50; i++ {
		tr.Start(string(rune('a'+i%26))+string(rune(i)), "t", nil)
	}
	if got := tr.Pending(); got > maxPending {
		t.Fatalf("Pending = %d, want <= %d", got, maxPending)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		msg  string
		want ErrKind
	}{
		{"success", nil, "", ErrNone},
		{"context canceled", context.Canceled, "", ErrCanceled},
		{"deadline", context.DeadlineExceeded, "", ErrTimeout},
		{"timeout text", nil, "request timed out after 5s", ErrTimeout},
		{"auth", nil, "401 Unauthorized", ErrAuth},
		{"not found", nil, `pods "business-ai-xxx" not found`, ErrNotFound},
		{"bad args", nil, "invalid parameter: namespace is required", ErrBadArgs},
		{"upstream", nil, "dial tcp: connection refused", ErrUpstream},
		{"unknown", nil, "something odd happened", ErrUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err, tc.msg); got != tc.want {
				t.Errorf("classify = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestToolErrorJudgement(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		want string
	}{
		{"nil", nil, ""},
		{"no key", map[string]any{"ok": true}, ""},
		{"nil value", map[string]any{"error": nil}, ""},
		{"empty string counts as success", map[string]any{"error": ""}, ""},
		{"string", map[string]any{"error": "boom"}, "boom"},
		{"error value", map[string]any{"error": errors.New("wrapped")}, "wrapped"},
		{"other type", map[string]any{"error": 42}, "42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToolError(tc.resp); got != tc.want {
				t.Errorf("ToolError = %q, want %q", got, tc.want)
			}
			if got, want := ToolFailed(tc.resp), tc.want != ""; got != want {
				t.Errorf("ToolFailed = %v, want %v", got, want)
			}
		})
	}
}
