package metrics

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
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

func TestSummaryAggregatesPerTool(t *testing.T) {
	tr, _ := newTestTracker(t)

	// Two slow failures and one fast success, mirroring a real run where a
	// search times out twice and a fetch succeeds.
	record := func(id, tool string, dur time.Duration, result map[string]any) {
		tr.Start(id, tool, nil)
		tr.mu.Lock()
		in := tr.pending[id]
		in.started = time.Now().Add(-dur)
		tr.pending[id] = in
		tr.mu.Unlock()
		tr.Finish(CallMeta{CallID: id, Tool: tool}, result, nil)
	}
	record("a", "web_search", 15*time.Second, map[string]any{"error": "context deadline exceeded"})
	record("b", "web_search", 15*time.Second, map[string]any{"error": "context deadline exceeded"})
	record("c", "fetch_url", 500*time.Millisecond, map[string]any{"title": "Example"})

	sum, err := tr.Summary(time.Time{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Calls != 3 {
		t.Fatalf("Calls = %d, want 3", sum.Calls)
	}
	if sum.Since.IsZero() {
		t.Error("Since is zero, want the oldest row's timestamp")
	}
	// Ordered by call count, so web_search (2) comes first.
	if len(sum.Tools) != 2 || sum.Tools[0].Tool != "web_search" {
		t.Fatalf("Tools = %+v, want web_search first", sum.Tools)
	}

	ws := sum.Tools[0]
	if ws.Calls != 2 || ws.OK != 0 {
		t.Errorf("web_search calls/ok = %d/%d, want 2/0", ws.Calls, ws.OK)
	}
	if ws.ErrKinds[string(ErrTimeout)] != 2 {
		t.Errorf("web_search err_kinds = %v, want 2 timeouts", ws.ErrKinds)
	}
	if ws.P50MS < 14_000 || ws.MaxMS < 14_000 {
		t.Errorf("web_search p50/max = %d/%d ms, want ~15000", ws.P50MS, ws.MaxMS)
	}

	fu := sum.Tools[1]
	if fu.Calls != 1 || fu.OK != 1 || len(fu.ErrKinds) != 0 {
		t.Errorf("fetch_url = %+v, want 1 call, 1 ok, no error kinds", fu)
	}
}

func TestSummaryHonoursSince(t *testing.T) {
	tr, _ := newTestTracker(t)
	tr.Start("old", "t", nil)
	tr.Finish(CallMeta{CallID: "old", Tool: "t"}, nil, nil)

	future, err := tr.Summary(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if future.Calls != 0 {
		t.Errorf("Calls = %d for a future window, want 0", future.Calls)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	cases := []struct {
		name   string
		sorted []int
		p      float64
		want   int
	}{
		{"empty", nil, 0.5, 0},
		{"single", []int{7}, 0.95, 7},
		// A tail statistic must not smooth away the only slow sample.
		{"three samples p95 is the max", []int{1, 2, 90}, 0.95, 90},
		{"median of four", []int{1, 2, 3, 4}, 0.5, 2},
		{"p95 of ten", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.95, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := percentile(tc.sorted, tc.p); got != tc.want {
				t.Errorf("percentile = %d, want %d", got, tc.want)
			}
		})
	}
}

// A column added to the schema is missing from every database created before
// it, because CREATE TABLE IF NOT EXISTS does nothing to an existing table.
// The insert then fails at runtime on a machine that had been running fine.
func TestRecorderMigratesAnOlderTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	// A database as an earlier version created it: no evidence_id, no replayed.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE tool_calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at TEXT NOT NULL, session_id TEXT NOT NULL DEFAULT '',
		invocation_id TEXT NOT NULL DEFAULT '', agent TEXT NOT NULL DEFAULT '',
		call_id TEXT NOT NULL DEFAULT '', tool TEXT NOT NULL,
		args TEXT NOT NULL DEFAULT '', duration_ms INTEGER NOT NULL DEFAULT 0,
		ok INTEGER NOT NULL, err_kind TEXT NOT NULL DEFAULT '',
		err TEXT NOT NULL DEFAULT '', result_bytes INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO tool_calls (at, tool, ok) VALUES ('2026-09-01T00:00:00Z', 'legacy', 1)`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	rec, err := NewRecorder(path)
	if err != nil {
		t.Fatalf("opening an older database failed: %v", err)
	}
	defer rec.Close()

	// The new columns are usable...
	if err := rec.RecordGatewayCall(GatewayCall{
		Tool: "k8s_get_pods", OK: true, EvidenceID: "e1",
		Args: map[string]any{"cluster": "prod-a"},
	}); err != nil {
		t.Fatalf("insert after migration failed: %v", err)
	}
	// ...and the existing row survived.
	var n int
	if err := rec.db.QueryRow(`SELECT count(*) FROM tool_calls`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows = %d, want the legacy row plus the new one", n)
	}

	// Migrating twice is a no-op.
	rec2, err := NewRecorder(path)
	if err != nil {
		t.Fatalf("reopening a migrated database failed: %v", err)
	}
	rec2.Close()
}

// The gateway's row is the authoritative one: arguments after injection, the
// canonical tool name, and the evidence a conclusion can cite.
func TestRecordGatewayCallStoresInjectedArgsAndEvidence(t *testing.T) {
	rec, err := NewRecorder(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	if err := rec.RecordGatewayCall(GatewayCall{
		SessionID: "s1", CallID: "c1",
		Tool: "k8s_get_pods", // canonical, not whichever alias the model used
		Args: map[string]any{
			"cluster":   "prod-a", // injected — the model never sent this
			"namespace": "payment",
			"selector":  "app=payment",
		},
		Duration: 350 * time.Millisecond, OK: true,
		ResultBytes: 1200, EvidenceID: "e1",
	}); err != nil {
		t.Fatal(err)
	}

	var tool, args, evidenceID string
	var ok, replayed int
	if err := rec.db.QueryRow(
		`SELECT tool, args, evidence_id, ok, replayed FROM tool_calls`,
	).Scan(&tool, &args, &evidenceID, &ok, &replayed); err != nil {
		t.Fatal(err)
	}
	if tool != "k8s_get_pods" {
		t.Errorf("tool = %q", tool)
	}
	// The stored arguments are what was sent, not what was asked for — that is
	// the whole difference between this row and one taken from ADK's callback.
	if !strings.Contains(args, "prod-a") || !strings.Contains(args, "payment") {
		t.Errorf("args = %q, want the injected values", args)
	}
	if evidenceID != "e1" {
		t.Errorf("evidence_id = %q; Seal's citations could not be checked against the record", evidenceID)
	}
	if ok != 1 || replayed != 0 {
		t.Errorf("ok = %d, replayed = %d", ok, replayed)
	}
}
