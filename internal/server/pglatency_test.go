package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"google.golang.org/adk/model"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/metrics"
	"github.com/jelly-agent/jelly-agent/internal/record"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// What the two hot paths cost on each database. Opt-in; prints, asserts little.
//
// This is the number the whole migration was argued about and never measured:
// a local SQLite read is a page cache hit, a PostgreSQL statement is a network
// round trip, and the projections here issue many small queries. Whether that
// matters is a question about this code and this deployment, not one that can
// be answered from the shape of the two databases.
//
// The thresholds it does assert are deliberately loose. The point is to have
// the numbers in front of a person, and to fail only if something has gone
// badly wrong rather than to pin a latency this test cannot control.
func TestHotPathLatency(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN to compare hot-path latency")
	}

	exclusive(t, dsn)

	const (
		sessions    = 24
		eventsEach  = 30
		resultsEach = 4
		samples     = 9
	)

	type result struct {
		kind            string
		timeline, tasks time.Duration
		sessionsPage    time.Duration
	}
	var results []result

	for _, ref := range []string{filepath.Join(t.TempDir(), "state.db"), dsn} {
		kind, _ := storage.KindOf(ref)
		s, first := seedForLatency(t, ref, sessions, eventsEach, resultsEach)
		results = append(results, result{
			kind:         string(kind),
			timeline:     median(t, samples, func() { get(t, s, "/api/sessions/"+first+"/timeline") }),
			tasks:        median(t, samples, func() { get(t, s, "/api/tasks?limit=20") }),
			sessionsPage: median(t, samples, func() { get(t, s, "/api/sessions?limit=20") }),
		})
	}

	t.Logf("每个库 %d 个会话 × %d 条事件，其中 %d 条带产物；每项取 %d 次的中位数",
		sessions, eventsEach, resultsEach, samples)
	t.Logf("%-10s %14s %14s %14s", "", "会话时间线", "任务列表", "会话列表")
	for _, r := range results {
		t.Logf("%-10s %14s %14s %14s", r.kind,
			r.timeline.Round(time.Microsecond),
			r.tasks.Round(time.Microsecond),
			r.sessionsPage.Round(time.Microsecond))
	}
	if len(results) == 2 {
		t.Logf("%-10s %13.1f× %13.1f× %13.1f×", "倍数",
			ratio(results[1].timeline, results[0].timeline),
			ratio(results[1].tasks, results[0].tasks),
			ratio(results[1].sessionsPage, results[0].sessionsPage))
	}

	// A page a person waits for. Loose on purpose — this is a guard against a
	// path that became a per-row round trip, not a latency budget.
	const tooSlow = 5 * time.Second
	for _, r := range results {
		for name, d := range map[string]time.Duration{
			"会话时间线": r.timeline, "任务列表": r.tasks, "会话列表": r.sessionsPage,
		} {
			if d > tooSlow {
				t.Errorf("%s 上的%s 要 %s —— 慢到不像是往返次数的问题", r.kind, name, d)
			}
		}
	}
}

func get(t *testing.T, s *Server, path string) {
	t.Helper()
	if w := do(t, s, "GET", path, ""); w.Code != http.StatusOK {
		t.Fatalf("%s → %d: %s", path, w.Code, w.Body.String())
	}
}

func median(t *testing.T, n int, fn func()) time.Duration {
	t.Helper()
	fn() // warm: the first call pays for connection setup and plan caching
	ds := make([]time.Duration, 0, n)
	for range n {
		start := time.Now()
		fn()
		ds = append(ds, time.Since(start))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func ratio(a, b time.Duration) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// seedForLatency fills one database with a plausible amount of history and
// returns a server over it plus the id of the session to project.
func seedForLatency(t *testing.T, ref string, sessions, eventsEach, resultsEach int) (*Server, string) {
	t.Helper()
	ctx := context.Background()

	svc, err := jellysession.New(ref)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Start from empty so the two databases hold the same history.
	for _, tbl := range []string{"events", "sessions", "tool_results", "tool_result_seq", "tool_calls"} {
		db.Exec(`DELETE FROM ` + tbl)
	}

	store, err := record.Open(ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	var first string
	for i := range sessions {
		id := fmt.Sprintf("lat-%03d", i)
		if first == "" {
			first = id
		}
		created, err := svc.Create(ctx, &adksession.CreateRequest{
			AppName: engine.AppName, UserID: engine.UserID, SessionID: id,
		})
		if err != nil {
			t.Fatal(err)
		}
		for j := range eventsEach {
			if err := svc.AppendEvent(ctx, created.Session, &adksession.Event{
				LLMResponse: model.LLMResponse{
					Content: genai.NewContentFromText(
						fmt.Sprintf("第 %d 轮：查询集群指标并分析异常", j), genai.RoleUser)},
				ID: fmt.Sprintf("%s-e%d", id, j), Author: "user",
				Timestamp: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
		}
		for j := range resultsEach {
			if _, err := store.Put(ctx, record.Record{
				Scope:        record.Scope{AppName: engine.AppName, UserID: engine.UserID, SessionID: id},
				InvocationID: fmt.Sprintf("inv%d", j), CallID: fmt.Sprintf("c%d", j),
				Tool: "query_range", At: time.Now(), Payload: []byte("metric series payload"),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"events", "sessions", "tool_results", "tool_result_seq", "tool_calls"} {
			db.Exec(`DELETE FROM ` + tbl)
		}
	})

	cfg := &config.Config{
		DefaultProvider: "test",
		Providers:       []config.Provider{{Name: "test", BaseURL: "http://x", APIKey: "sk-test", Model: "m"}},
		Storage:         config.Storage{DSN: ref},
	}
	cfg.Memory.Core.Dir = t.TempDir()
	eng := engine.New(cfg)
	rec, err := metrics.NewRecorder(ref)
	if err != nil {
		t.Fatal(err)
	}
	tr := metrics.NewTracker(rec)
	t.Cleanup(func() { tr.Close(); eng.Close() })
	eng.SetMetrics(tr)
	return New(eng, nil), first
}
