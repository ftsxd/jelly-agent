package server

import (
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/metrics"
)

// /api/stats merges two sources with different coverage: call counts scanned
// out of session events (all history) and timing recorded in tool_calls (only
// since the hooks existed). The response must carry both, and must not drop a
// tool that has timing rows but no events — that gap is precisely what the
// merge is there to expose.
func TestStatsReportsRecordedTiming(t *testing.T) {
	s := newTestServer(t)
	tr := s.engine().Metrics()

	// One fast success and two slow timeouts, the shape a failing search has.
	for _, c := range []struct {
		id, tool string
		took     time.Duration
		result   map[string]any
	}{
		{"a", "fetch_url", 500 * time.Millisecond, map[string]any{"title": "Example"}},
		{"b", "web_search", 15 * time.Second, map[string]any{"error": "context deadline exceeded"}},
		{"c", "web_search", 15 * time.Second, map[string]any{"error": "context deadline exceeded"}},
	} {
		tr.Start(c.id, c.tool, nil)
		tr.Finish(metrics.CallMeta{CallID: c.id, Tool: c.tool}, c.result, nil)
	}

	w := do(t, s, "GET", "/api/stats", "")
	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	body := decode(t, w)

	tele, ok := body["telemetry"].(map[string]any)
	if !ok {
		t.Fatalf("telemetry missing from %v", body)
	}
	if got := tele["calls"].(float64); got != 3 {
		t.Errorf("telemetry.calls = %v, want 3", got)
	}
	if tele["since"] == "" || tele["since"] == nil {
		t.Error("telemetry.since is empty, want the oldest recorded call")
	}

	tools, _ := body["tools"].([]any)
	byName := map[string]map[string]any{}
	for _, raw := range tools {
		m := raw.(map[string]any)
		byName[m["name"].(string)] = m
	}

	ws, ok := byName["web_search"]
	if !ok {
		t.Fatalf("web_search missing; a tool with rows but no session events must still be listed, got %v", byName)
	}
	if got := ws["timed"].(float64); got != 2 {
		t.Errorf("web_search.timed = %v, want 2", got)
	}
	if got := ws["ok"].(float64); got != 0 {
		t.Errorf("web_search.ok = %v, want 0", got)
	}
	kinds, _ := ws["err_kinds"].(map[string]any)
	if kinds[string(metrics.ErrTimeout)] != float64(2) {
		t.Errorf("web_search.err_kinds = %v, want 2 timeouts", kinds)
	}

	fu := byName["fetch_url"]
	if fu == nil || fu["ok"].(float64) != 1 {
		t.Errorf("fetch_url = %v, want one successful call", fu)
	}
	// Counts from session events stay zero here: this server has no sessions,
	// which is exactly the "timed but never seen in events" case.
	if got := fu["count"].(float64); got != 0 {
		t.Errorf("fetch_url.count = %v, want 0 (no session events in this server)", got)
	}
}
