package migrate_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/adk/model"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/metrics"
	"github.com/jelly-agent/jelly-agent/internal/migrate"
	"github.com/jelly-agent/jelly-agent/internal/record"
	"github.com/jelly-agent/jelly-agent/internal/schedule"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	"github.com/jelly-agent/jelly-agent/internal/storage"
	"github.com/jelly-agent/jelly-agent/internal/task"
)

const (
	app  = engine.AppName
	user = engine.UserID
	sess = "migrated-session"
)

// A SQLite deployment with something in every table, moved to PostgreSQL and
// read back through the stores. Opt-in.
//
// Reading it back through the stores rather than comparing rows is the point:
// a copy that lands the right bytes in the wrong type is still broken, and
// only the code that knows what a column means can tell. SQLite keeps booleans
// as 0 and 1; PostgreSQL's are real booleans and reject an integer.
func TestMigrateSQLiteToPostgres(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN to exercise the migration")
	}
	ctx := context.Background()

	srcPath := filepath.Join(t.TempDir(), "state.db")
	seeded := seedSQLite(t, srcPath)

	src, err := storage.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := freshPostgres(t, dsn)

	rep, err := migrate.Run(ctx, src, dst, migrate.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, table := range rep.Order {
		t.Logf("%-16s 复制 %d，跳过 %d", table, rep.Copied[table], rep.Skipped[table])
	}

	bad, err := migrate.Verify(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range bad {
		t.Errorf("行数对不上 —— %s", m)
	}

	t.Run("产物能通过句柄读回来", func(t *testing.T) {
		store, err := record.Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		sc := record.Scope{AppName: app, UserID: user, SessionID: sess}
		got, err := store.ReadLabel(ctx, sc, seeded.label, 0, 1000)
		if err != nil {
			t.Fatalf("ReadLabel(%s): %v", seeded.label, err)
		}
		if string(got.Data) != seeded.payload {
			t.Errorf("payload = %q, want %q", got.Data, seeded.payload)
		}
	})

	t.Run("下一条交付接着编号，不复用", func(t *testing.T) {
		// tool_result_seq came across, so the counter continues rather than
		// starting over and handing out a handle that already names something.
		store, err := record.Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		next, err := store.Put(ctx, record.Record{
			Scope:        record.Scope{AppName: app, UserID: user, SessionID: sess},
			InvocationID: "inv2", CallID: "after-migration",
			Tool: "query_range", At: time.Now(), Payload: []byte("x"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if next == seeded.label {
			t.Errorf("迁移后第一条交付拿到 %q —— 和已有的句柄撞了", next)
		}
	})

	t.Run("布尔列迁过来还是布尔", func(t *testing.T) {
		rec, err := metrics.NewRecorder(dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rec.Close() })
		rows, err := rec.ByInvocation(sess, "inv1")
		if err != nil {
			t.Fatalf("ByInvocation: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("读回 %d 行，want 1", len(rows))
		}
		if !rows[0].OK {
			t.Error("ok 迁过来变成了 false —— SQLite 的 1 没有变成 true")
		}
	})

	t.Run("任务归属与排程记录", func(t *testing.T) {
		links, err := task.OfSession(dst, sess)
		if err != nil || links["inv1"] == "" {
			t.Errorf("OfSession = %v, %v", links, err)
		}
		runs, total, err := schedule.List(dst, "nightly", 10, 0)
		if err != nil || total != 1 || len(runs) != 1 {
			t.Fatalf("schedule.List = %d/%d, %v", len(runs), total, err)
		}
		if runs[0].SessionID != sess {
			t.Errorf("排程记录的 session = %q", runs[0].SessionID)
		}
	})

	t.Run("重跑是幂等的", func(t *testing.T) {
		again, err := migrate.Run(ctx, src, dst, migrate.Options{})
		if err != nil {
			t.Fatalf("second run: %v", err)
		}
		for _, table := range again.Order {
			if again.Copied[table] != 0 {
				t.Errorf("%s 第二遍又复制了 %d 行", table, again.Copied[table])
			}
		}
	})
}

type seedResult struct {
	label   string
	payload string
}

// seedSQLite builds a deployment with something in every table Tables lists.
func seedSQLite(t *testing.T, path string) seedResult {
	t.Helper()
	ctx := context.Background()

	svc, err := jellysession.New(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: app, UserID: user, SessionID: sess,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AppendEvent(ctx, created.Session, &adksession.Event{
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("查一下集群内存", genai.RoleUser)},
		ID:          "ev1", Author: "user", Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	e := engine.New(&config.Config{Storage: config.Storage{DSN: path}})
	t.Cleanup(e.Close)
	db, err := e.StateDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Link(db, task.ID(sess, "inv1"), sess, "inv1"); err != nil {
		t.Fatal(err)
	}
	if err := schedule.Record(db, "nightly", time.Now().Add(-time.Minute),
		"succeeded", "巡检完成", "", sess, "inv1"); err != nil {
		t.Fatal(err)
	}

	rec, err := metrics.NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	// OK true, so the boolean assertion after the migration means something:
	// SQLite stores it as 1 and PostgreSQL's column rejects an integer.
	if err := rec.Record(metrics.ToolCall{
		SessionID: sess, InvocationID: "inv1", CallID: "c1", Tool: "query_range",
		Agent: "root", Duration: 12 * time.Millisecond, OK: true, At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	store, err := record.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	const payload = "第一行\n第二行\n第三行\n"
	label, err := store.Put(ctx, record.Record{
		Scope:        record.Scope{AppName: app, UserID: user, SessionID: sess},
		InvocationID: "inv1", CallID: "c1", Tool: "query_range",
		At: time.Now(), Payload: []byte(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	return seedResult{label: label, payload: payload}
}

// freshPostgres empties the target and gives back a handle on it.
//
// Emptied rather than dropped: the schema is migrations/postgres/0001_init.sql
// plus ADK's AutoMigrate, and a test that recreated it would be testing its own
// copy of the schema instead of the one deployments run.
func freshPostgres(t *testing.T, dsn string) *storage.DB {
	t.Helper()
	if _, err := jellysession.New(dsn); err != nil { // ADK's four tables
		t.Fatal(err)
	}
	db, err := storage.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	empty := func() {
		for i := len(migrate.Tables) - 1; i >= 0; i-- { // children before parents
			if _, err := db.Exec(`DELETE FROM ` + migrate.Tables[i]); err != nil {
				t.Fatalf("clear %s: %v", migrate.Tables[i], err)
			}
		}
		db.Exec(`DELETE FROM memory_fts`)
	}
	empty()
	t.Cleanup(empty)
	return db
}

// Verify has to report a difference, not just be called.
//
// The migration test calls it and asserts nothing came back, which passes
// equally well if it never reports anything at all — so this feeds it two
// databases that differ and checks it says so.
func TestVerifyReportsARowCountDifference(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN")
	}
	ctx := context.Background()

	srcPath := filepath.Join(t.TempDir(), "state.db")
	seedSQLite(t, srcPath)
	src, err := storage.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := freshPostgres(t, dsn) // emptied, so every table differs

	bad, err := migrate.Verify(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) == 0 {
		t.Fatal("源有数据、目标是空的，Verify 却说一致")
	}
	var sawResults bool
	for _, m := range bad {
		if m.Table == "tool_results" {
			sawResults = true
			if m.Source == 0 || m.Target != 0 {
				t.Errorf("报告的数字不对: %s", m)
			}
		}
	}
	if !sawResults {
		t.Errorf("没报 tool_results: %v", bad)
	}

	// And a target that is ahead is a different mistake, reported as one.
	if _, err := dst.Exec(
		`INSERT INTO task_runs (task_id, session_id, invocation_id, at) VALUES (?,?,?,?)`,
		"extra/inv", "extra", "inv", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	bad, err = migrate.Verify(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range bad {
		if m.Table == "task_runs" && m.Target <= m.Source {
			t.Errorf("目标多出来的行没被如实报告: %s", m)
		}
	}
}

// A column the source has and the target does not must be skipped, not fail
// the copy.
//
// This is what lets a migration run while the two schema definitions are one
// commit apart — and without it the copier builds an INSERT naming a column
// the target has never heard of, which fails the whole table.
func TestColumnOnlyInTheSourceIsSkipped(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN")
	}
	ctx := context.Background()

	srcPath := filepath.Join(t.TempDir(), "state.db")
	seedSQLite(t, srcPath)
	src, err := storage.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(
		`ALTER TABLE task_runs ADD COLUMN only_here TEXT NOT NULL DEFAULT 'x'`); err != nil {
		t.Fatal(err)
	}
	dst := freshPostgres(t, dsn)

	rep, err := migrate.Run(ctx, src, dst, migrate.Options{})
	if err != nil {
		t.Fatalf("多出一列就让整张表迁不动了: %v", err)
	}
	if rep.Copied["task_runs"] != 1 {
		t.Errorf("task_runs 复制了 %d 行，want 1", rep.Copied["task_runs"])
	}
}
