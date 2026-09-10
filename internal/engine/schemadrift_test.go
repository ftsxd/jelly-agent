package engine

import (
	"github.com/jelly-agent/jelly-agent/internal/migrate"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
	jellymetrics "github.com/jelly-agent/jelly-agent/internal/metrics"
	"github.com/jelly-agent/jelly-agent/internal/record"
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

	exclusive(t, dsn)

	// SQLite is built by running every store's initialiser, because that is
	// how a SQLite deployment gets its schema.
	lite := buildSQLiteSchema(t, filepath.Join(t.TempDir(), "state.db"))

	// PostgreSQL is only read. Running the initialisers against it would let
	// this test repair the very drift it exists to find: each store's
	// EnsureColumns list is checked and applied, so a column added to both the
	// SQLite DDL and that list — but not to the migration — would be ALTERed
	// into PostgreSQL here and then compare equal. Green, and the deployment
	// still missing the column.
	pg, err := storage.Open(dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { pg.Close() })

	// Derived from what the migration command copies rather than listed
	// here. A hand-written list is a third place to forget a table, and
	// forgetting one means its two definitions drift with nothing watching.
	//
	// ADK's four are excluded: they are created by GORM AutoMigrate on both
	// sides from one set of models, so they cannot drift the way two
	// hand-written definitions can. memory_fts is added because it is not
	// copied — it is a derived index, rebuilt rather than migrated — but its
	// two definitions are still hand-written.
	adk := map[string]bool{"sessions": true, "events": true, "app_states": true, "user_states": true}
	tables := []string{"memory_fts"}
	for _, t := range migrate.Tables {
		if !adk[t] {
			tables = append(tables, t)
		}
	}
	for _, table := range tables {
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

// buildSQLiteSchema opens a SQLite file and creates every store's schema in
// it, the way a SQLite deployment does — including the two stores that keep
// their own handle and so are not covered by engine.StateDB.
func buildSQLiteSchema(t *testing.T, path string) *storage.DB {
	t.Helper()
	e := New(&config.Config{Storage: config.Storage{DSN: path}})
	t.Cleanup(e.Close)

	db, err := e.StateDB() // session, task, schedule, memory
	if err != nil {
		t.Fatalf("StateDB: %v", err)
	}
	store, err := record.Open(path)
	if err != nil {
		t.Fatalf("record.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	rec, err := jellymetrics.NewRecorder(path)
	if err != nil {
		t.Fatalf("metrics.NewRecorder: %v", err)
	}
	t.Cleanup(func() { rec.Close() })
	return db
}
