package memory

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	adkmemory "google.golang.org/adk/memory"
	"google.golang.org/adk/model"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// L2 search against a real PostgreSQL. Opt-in.
//
// Everything else about this store is portable; the search is not. SQLite
// answers it with FTS5's trigram MATCH and its own rank, PostgreSQL with ILIKE
// over a GIN trigram index ordered by similarity(). Two implementations, so
// the SQLite tests say nothing about the other one.
func TestSearchAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN to exercise L2 search against PostgreSQL")
	}
	exclusive(t, dsn)
	ctx := context.Background()

	svc, _, err := jellysession.New(dsn)
	if err != nil {
		t.Fatalf("session service: %v", err)
	}
	s, err := NewSearch(dsn, 5)
	if err != nil {
		t.Fatalf("NewSearch: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if got := s.db.Kind(); got != storage.KindPostgres {
		t.Fatalf("handle is on %s, not postgres", got)
	}

	const sess = "pgsearch"
	t.Cleanup(func() {
		PurgeSessions(s.db, []string{sess})
		svc.Delete(ctx, &adksession.DeleteRequest{
			AppName: testApp, UserID: testUser, SessionID: sess,
		})
	})
	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: testApp, UserID: testUser, SessionID: sess,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Set explicitly: ADK's own constructors stamp this, but an event literal
	// does not, and an unstamped one would make the round-trip assertion below
	// pass against a zero on both sides.
	stamped := time.Now().UTC().Truncate(time.Second)
	for i, text := range []string{
		"集群节点内存使用率持续告警，需要扩容",
		"日志采集正常，无异常",
		"redis 实例 algo-data 的连接数偏高",
		// Three rows that all contain "内存使用率", so ordering is observable.
		// The closest one is the shortest: pg_trgm's similarity is the overlap
		// of trigram sets, so a row that is little more than the query scores
		// higher than one where it is a fragment.
		"内存使用率",
		"上周三下午巡检时发现，若干台机器的内存使用率略有上升，但都在阈值以内，暂时不需要处理",
	} {
		ev := &adksession.Event{
			LLMResponse: model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleUser)},
			ID:          string(rune('a'+i)) + "-ev",
			Author:      "user",
			Timestamp:   stamped,
		}
		if err := svc.AppendEvent(ctx, created.Session, ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddSessionToMemory(ctx, created.Session); err != nil {
		t.Fatalf("AddSessionToMemory: %v", err)
	}

	hits := func(t *testing.T, q string) []string {
		t.Helper()
		resp, err := s.SearchMemory(ctx, &adkmemory.SearchRequest{
			AppName: testApp, UserID: testUser, Query: q,
		})
		if err != nil {
			t.Fatalf("SearchMemory(%q): %v", q, err)
		}
		var out []string
		for _, m := range resp.Memories {
			out = append(out, m.Content.Parts[0].Text)
		}
		return out
	}

	t.Run("长查询走 ILIKE + trgm 索引", func(t *testing.T) {
		got := hits(t, "内存使用率")
		if len(got) != 3 {
			t.Fatalf("命中 %d 条，want 3: %v", len(got), got)
		}
	})

	t.Run("长查询按 similarity 排序", func(t *testing.T) {
		// Standing in for FTS5's rank. Without an ORDER BY that scores the
		// hits, this is whatever order the rows come back in — which on three
		// matching rows is indistinguishable from working.
		got := hits(t, "内存使用率")
		if len(got) == 0 {
			t.Fatal("no hits")
		}
		if got[0] != "内存使用率" {
			t.Errorf("排在第一的是 %q，最接近的应该是与查询几乎相同的那条", got[0])
		}
		// And the longest, where the query is a small fragment, is last.
		if !strings.Contains(got[len(got)-1], "巡检") {
			t.Errorf("排在最后的是 %q，应该是查询占比最小的那条", got[len(got)-1])
		}
	})

	t.Run("英文也能命中", func(t *testing.T) {
		if got := hits(t, "algo-data"); len(got) != 1 {
			t.Errorf("got %v", got)
		}
	})

	t.Run("短查询（<3 字）走按时间的扫描", func(t *testing.T) {
		// The trigram index cannot answer these, on either database. It has to
		// still return the right rows rather than nothing.
		got := hits(t, "扩容")
		if len(got) != 1 || !strings.Contains(got[0], "内存") {
			t.Errorf("got %v", got)
		}
	})

	t.Run("时间戳能读回来", func(t *testing.T) {
		resp, err := s.SearchMemory(ctx, &adkmemory.SearchRequest{
			AppName: testApp, UserID: testUser, Query: "内存使用率",
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Memories) == 0 {
			t.Fatal("no hits")
		}
		// Written as RFC3339 text and parsed back as RFC3339 text, through
		// PostgreSQL's column. If that column were a real timestamp type the
		// value would come back in the database's own format and parse to
		// zero — which is how the same mismatch got past the store-level test
		// for tool_results.at.
		if got := resp.Memories[0].Timestamp; !got.Equal(stamped) {
			t.Errorf("时间戳 = %s, want %s —— ts 的格式两边对不上", got, stamped)
		}
	})

	t.Run("不跨 app/user", func(t *testing.T) {
		resp, err := s.SearchMemory(ctx, &adkmemory.SearchRequest{
			AppName: testApp, UserID: "somebody-else", Query: "内存使用率",
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Memories) != 0 {
			t.Errorf("另一个用户搜到了 %d 条", len(resp.Memories))
		}
	})
}
