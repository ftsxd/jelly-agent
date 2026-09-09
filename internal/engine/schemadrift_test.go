package engine

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
	jellymetrics "github.com/jelly-agent/jelly-agent/internal/metrics"
	"github.com/jelly-agent/jelly-agent/internal/record"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// The two schemas are two independent definitions, and this is what keeps them
// the same.
//
// Each store carries a DDL string it runs on SQLite; PostgreSQL's schema is
// migrations/postgres/0001_init.sql, written by hand. Add a column to one and
// forget the other and nothing complains — storage.ApplySchema checks that a
// table exists, not what is in it, and it cannot do more: a PostgreSQL
// deployment never runs the SQLite DDL, so only a test with both databases in
// front of it can compare them.
//
// Names only, not types. INTEGER against boolean and TEXT against text are
// deliberate — see the notes in migrations/README.md. A column that exists on
// one side and not the other never is.
func TestSQLiteAndPostgresSchemasAgree(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN to compare the two schemas")
	}

	lite := openEverything(t, filepath.Join(t.TempDir(), "state.db"))
	pg := openEverything(t, dsn)

	// ADK's four are not compared: they are created by GORM AutoMigrate on
	// both sides from one set of models, so they cannot drift the way two
	// hand-written definitions can.
	for _, table := range []string{
		"tool_results", "tool_result_seq", "tool_calls",
		"task_runs", "schedule_runs", "memory_fts",
	} {
		t.Run(table, func(t *testing.T) {
			a, err := storage.Columns(lite, table)
			if err != nil {
				t.Fatalf("SQLite: %v", err)
			}
			b, err := storage.Columns(pg, table)
			if err != nil {
				t.Fatalf("PostgreSQL: %v", err)
			}
			if len(a) == 0 {
				t.Fatalf("SQLite 上 %s 没有列 —— 这个表根本没建出来", table)
			}
			for _, c := range a {
				if !slices.Contains(b, c) {
					t.Errorf("%s.%s 只在 SQLite 上有 —— migrations/postgres/0001_init.sql 少了它", table, c)
				}
			}
			for _, c := range b {
				if !slices.Contains(a, c) {
					t.Errorf("%s.%s 只在 PostgreSQL 上有 —— 某个 store 的 DDL 少了它", table, c)
				}
			}
		})
	}
}

// openEverything opens one database and creates every store's schema in it —
// including the two that keep their own handle and so are not covered by
// engine.StateDB.
func openEverything(t *testing.T, ref string) *storage.DB {
	t.Helper()
	e := New(&config.Config{Storage: config.Storage{DSN: ref}})
	t.Cleanup(e.Close)

	db, err := e.StateDB() // session, task, schedule, memory
	if err != nil {
		t.Fatalf("StateDB: %v", err)
	}
	store, err := record.Open(ref)
	if err != nil {
		t.Fatalf("record.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	rec, err := jellymetrics.NewRecorder(ref)
	if err != nil {
		t.Fatalf("metrics.NewRecorder: %v", err)
	}
	t.Cleanup(func() { rec.Close() })

	// ADK's tables, so a comparison run against a database that has never had
	// them is not comparing against half a schema.
	if _, err := jellysession.New(ref); err != nil {
		t.Fatalf("session service: %v", err)
	}
	return db
}
