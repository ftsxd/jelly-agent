package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDialectForReference(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  string
		want any
	}{
		{"plain SQLite path", filepath.Join("state", "state.db"), sqliteDialect{}},
		{"absolute SQLite path", filepath.Join(t.TempDir(), "state.db"), sqliteDialect{}},
		{"postgres scheme", "postgres://u:p@db.example/jelly", postgresDialect{}},
		{"postgresql scheme", "postgresql://u:p@db.example/jelly", postgresDialect{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dialectFor(tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%T", got) != fmt.Sprintf("%T", tc.want) {
				t.Errorf("dialect = %T, want %T", got, tc.want)
			}
		})
	}
}

func TestDialectForReferenceRejectsUnknownScheme(t *testing.T) {
	if _, err := dialectFor("mysql://db.example/jelly"); err == nil {
		t.Fatal("an unsupported URL was treated as a SQLite path")
	}
}

func TestOpenPostgresUsesPgxAndABoundedPool(t *testing.T) {
	// database/sql opens lazily, so this verifies selection, driver
	// registration and pool policy without requiring a server.
	db, err := Open("postgres://user:secret@db.invalid/jelly")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, ok := db.dialect.(postgresDialect); !ok {
		t.Fatalf("dialect = %T, want postgresDialect", db.dialect)
	}
	if got := db.db.Stats().MaxOpenConnections; got != postgresConnLimit {
		t.Fatalf("max open connections = %d, want %d", got, postgresConnLimit)
	}
}

func TestPostgresRebindNumbersOnlyPlaceholders(t *testing.T) {
	q := `SELECT ? FROM "odd?table" WHERE note='why?' AND x=? -- keep ?
		AND y IN (?,?) /* keep ? */`
	want := `SELECT $1 FROM "odd?table" WHERE note='why?' AND x=$2 -- keep ?
		AND y IN ($3,$4) /* keep ? */`
	if got := (postgresDialect{}).rebind(q); got != want {
		t.Errorf("\ngot  %s\nwant %s", got, want)
	}
}

func TestPostgresUniqueViolationUsesSQLState(t *testing.T) {
	conflict := &pgconn.PgError{Code: "23505", Message: "duplicate key"}
	if !IsUniqueViolation(conflict) {
		t.Fatal("23505 was not classified as a uniqueness clash")
	}
	if !IsUniqueViolation(fmt.Errorf("record: put: %w", conflict)) {
		t.Fatal("wrapping hid PostgreSQL's SQLSTATE")
	}
	if IsUniqueViolation(&pgconn.PgError{Code: "23503", Message: "foreign key"}) {
		t.Fatal("a foreign-key violation was classified as unique")
	}
}

// The unit tests above need no server. This opt-in probe crosses the actual
// database/sql → pgx → PostgreSQL boundary, including rebind and the
// information_schema query. The repository's development database is already
// initialized with tool_results, so no test DDL or cleanup is needed.
func TestPostgresDialectAgainstRealDatabase(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN to test the storage dialect against PostgreSQL")
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var got int
	if err := db.QueryRow(`SELECT ?::integer`, 7).Scan(&got); err != nil {
		t.Fatalf("rebound query: %v", err)
	}
	if got != 7 {
		t.Fatalf("query returned %d, want 7", got)
	}
	has, err := db.dialect.hasColumn(db, "tool_results", "seq")
	if err != nil {
		t.Fatalf("inspect column: %v", err)
	}
	if !has {
		t.Fatal("initialized PostgreSQL has no tool_results.seq")
	}
	if has, err := db.dialect.hasColumn(db, "tool_results", "no_such_column"); err != nil || has {
		t.Fatalf("a column that does not exist: %v, %v", has, err)
	}

	// A same-named table in another schema must not answer for this one.
	//
	// information_schema.columns spans every schema the connection can see,
	// so a query without the table_schema filter finds a decoy — and reports
	// a column as present that this schema does not have, which means
	// EnsureColumns skips an ALTER the code depends on.
	t.Run("另一个 schema 里的同名表不算", func(t *testing.T) {
		for _, ddl := range []string{
			`CREATE SCHEMA IF NOT EXISTS decoy`,
			`CREATE TABLE IF NOT EXISTS decoy.tool_results (only_here text)`,
		} {
			if _, err := db.Exec(ddl); err != nil {
				t.Skipf("需要建 schema 的权限: %v", err)
			}
		}
		t.Cleanup(func() { db.Exec(`DROP SCHEMA decoy CASCADE`) })

		has, err := db.dialect.hasColumn(db, "tool_results", "only_here")
		if err != nil {
			t.Fatal(err)
		}
		if has {
			t.Error("decoy.tool_results 的列被当成了本 schema 的列")
		}
	})
}

// A missing table has to be recognised on both dialects.
//
// Three callers tolerate it — a store whose schema is not there yet holds
// nothing, so listing or purging it is a no-op — and all three matched
// SQLite's message text. On PostgreSQL that is not a match, so a fresh
// deployment's first sessions-page load answered 500 with a SQL error in it,
// until something happened to open ADK's session service first and create the
// tables. The end-to-end test could not see it because it opens that service
// in its first subtest.
func TestMissingTableIsRecognisedOnBothDialects(t *testing.T) {
	if !IsMissingTable(&pgconn.PgError{Code: "42P01", Message: `relation "sessions" does not exist`}) {
		t.Error("PostgreSQL 的 42P01 没被识别成缺表")
	}
	if !IsMissingTable(fmt.Errorf("list sessions: %w",
		&pgconn.PgError{Code: "42P01"})) {
		t.Error("包一层之后识别不出来了")
	}
	if IsMissingTable(&pgconn.PgError{Code: "42703", Message: "column does not exist"}) {
		t.Error("缺列被当成了缺表")
	}
	if IsMissingTable(nil) {
		t.Error("nil 被当成了缺表")
	}

	db, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Query(`SELECT 1 FROM nosuchtable`)
	if !IsMissingTable(err) {
		t.Errorf("SQLite 的缺表没被识别: %v", err)
	}
}
