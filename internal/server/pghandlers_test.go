package server

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/metrics"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// The handlers, against a real PostgreSQL. Opt-in.
//
// internal/engine's end-to-end test exercises the stores; this one goes
// through HTTP, which is where the projections, the paging and the evidence
// endpoints live. They build their own SQL, so a dialect gap in any of them is
// invisible to a store-level test — and invisible to every test on SQLite,
// where `?` is the native placeholder.
func newPostgresServer(t *testing.T) (*Server, string) {
	t.Helper()
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN to exercise the handlers against PostgreSQL")
	}
	exclusive(t, dsn)
	cfg := &config.Config{
		DefaultProvider: "test",
		Providers:       []config.Provider{{Name: "test", BaseURL: "http://x", APIKey: "sk-test", Model: "m"}},
		Storage:         config.Storage{DSN: dsn},
	}
	cfg.Memory.Core.Dir = t.TempDir()
	eng := engine.New(cfg)
	rec, err := metrics.NewRecorder(dsn)
	if err != nil {
		t.Fatalf("metrics on postgres: %v", err)
	}
	tr := metrics.NewTracker(rec)
	t.Cleanup(func() { tr.Close(); eng.Close() })
	eng.SetMetrics(tr)

	// ADK owns sessions and events, and creates them on first open.
	if _, err := jellysession.New(dsn); err != nil {
		t.Fatalf("session service: %v", err)
	}
	return New(eng, nil), dsn
}

func TestHandlersWorkAgainstPostgres(t *testing.T) {
	s, _ := newPostgresServer(t)

	for _, tc := range []struct {
		name, method, path string
	}{
		{"会话列表", "GET", "/api/sessions?limit=5"},
		{"会话 id 全集", "GET", "/api/sessions/ids"},
		{"任务列表", "GET", "/api/tasks?limit=5"},
		{"排程运行", "GET", "/api/schedules/runs?limit=5"},
		{"概览统计（走 metrics.Summary）", "GET", "/api/stats"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, s, tc.method, tc.path, "")
			if w.Code == http.StatusNotFound {
				t.Skipf("端点不存在: %s", tc.path)
			}
			if w.Code != http.StatusOK {
				t.Errorf("status = %d: %s", w.Code, w.Body.String())
			}
			var any map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &any); err != nil {
				var arr []json.RawMessage
				if err2 := json.Unmarshal(w.Body.Bytes(), &arr); err2 != nil {
					t.Errorf("返回不是 JSON: %s", w.Body.String())
				}
			}
		})
	}
}

// The migration has run, but ADK's session service never has.
//
// This is the state the store-level end-to-end test cannot reach, because it
// opens that service in its first step. And it is reachable in practice: the
// migration file deliberately does not define sessions and events — those are
// ADK's, created by AutoMigrate — so an operator who runs the migration and
// then loads the console arrives here.
//
// Every read path has to treat the absent table as "nothing yet" rather than
// as a failure. On SQLite that worked by matching the driver's message; on
// PostgreSQL the same text never matches, so the console answered 500 with a
// SQL error in it.
func TestSessionReadsToleratePostgresWithoutADKTables(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN")
	}
	dropADKTables(t, dsn)

	exclusive(t, dsn)
	cfg := &config.Config{
		DefaultProvider: "test",
		Providers:       []config.Provider{{Name: "test", BaseURL: "http://x", APIKey: "sk-test", Model: "m"}},
		Storage:         config.Storage{DSN: dsn},
	}
	cfg.Memory.Core.Dir = t.TempDir()
	eng := engine.New(cfg)
	t.Cleanup(eng.Close)
	s := New(eng, nil)

	for _, path := range []string{
		"/api/sessions?limit=5",
		"/api/sessions/ids",
		"/api/tasks?limit=5",
	} {
		t.Run(path, func(t *testing.T) {
			w := do(t, s, "GET", path, "")
			if w.Code != http.StatusOK {
				t.Errorf("ADK 的表还不存在时 status = %d: %s", w.Code, w.Body.String())
			}
		})
	}

	// And a delete, which is the path that has to roll back and report success.
	w := do(t, s, "POST", "/api/sessions/delete", `{"ids":["never-existed"]}`)
	if w.Code != http.StatusOK {
		t.Errorf("删除 status = %d: %s", w.Code, w.Body.String())
	}
}

// dropADKTables removes the tables ADK creates, leaving the ones the migration
// file defines. Restored by reopening the service afterwards.
func dropADKTables(t *testing.T, dsn string) {
	t.Helper()
	db, err := storage.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tbl := range []string{"events", "sessions", "app_states", "user_states"} {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + tbl + ` CASCADE`); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	t.Cleanup(func() {
		if _, err := jellysession.New(dsn); err != nil {
			t.Logf("恢复 ADK 表失败: %v", err)
		}
	})
}
