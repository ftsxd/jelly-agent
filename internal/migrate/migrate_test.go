package migrate_test

import (
	"context"
	"errors"
	"github.com/jelly-agent/jelly-agent/internal/toolreg"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

	exclusive(t, dsn)
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

	// Before any subtest writes to the target: resuming is about a copy that
	// was interrupted, and by definition the service has not started yet.
	t.Run("重跑是幂等的", func(t *testing.T) {
		again, err := migrate.Run(ctx, src, dst, migrate.Options{})
		if err != nil {
			t.Fatalf("second run: %v", err)
		}
		for _, table := range again.Order {
			if again.Copied[table] != 0 {
				t.Errorf("%s 第二遍又复制了 %d 行", table, again.Copied[table])
			}
			if again.Skipped[table] != rep.Copied[table] {
				t.Errorf("%s 第一遍搬了 %d 行，第二遍只认出 %d 行已存在",
					table, rep.Copied[table], again.Skipped[table])
			}
		}
	})

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

	t.Run("控制台声明与它的审计历史", func(t *testing.T) {
		// The console's layer is state now, not a file. A migration that left
		// it behind would silently drop every declaration an operator made —
		// which is the layer that decides how tools get scored.
		metas, err := toolreg.NewDBSource(dst).Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, m := range metas {
			if m.Name == "query_range" && slices.Contains(m.Suites, "promql") {
				found = true
			}
		}
		if !found {
			t.Errorf("控制台声明没有迁过来: %+v", metas)
		}
		hist, err := toolreg.History(ctx, dst, "n9e-mcp", "query_range", 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 1 || hist[0].ChangedBy != "alice" {
			t.Errorf("审计历史没有迁过来: %+v", hist)
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

	t.Run("目标库被写过之后，重跑会说出来", func(t *testing.T) {
		// The subtests above opened the stores against the target and wrote
		// through them, which is what a deployment does the moment it comes
		// up on the new database. Re-running the migration then is a mistake
		// — the target has moved on — and it has to be said rather than
		// skipped over as "已存在".
		_, err := migrate.Run(ctx, src, dst, migrate.Options{})
		var ce *migrate.ConflictError
		if !errors.As(err, &ce) {
			t.Fatalf("目标库已经被服务写过，重跑却报成功: %v", err)
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

	svc, _, err := jellysession.New(path)
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

	if err := toolreg.SaveDecl(ctx, db, toolreg.Decl{
		Server: "n9e-mcp", Name: "query_range", Suites: &[]string{"promql"},
	}, "alice"); err != nil {
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
	if _, _, err := jellysession.New(dsn); err != nil { // ADK's four tables
		t.Fatal(err)
	}
	db, err := storage.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Sequences too, not just rows. DELETE leaves an identity counter where
	// it was, so a target emptied but not reset starts ahead of the ids the
	// migration copies — which hides exactly the bug that the migration has
	// to advance them itself. "Fresh" has to mean what a new database means.
	resetSequences := func() {
		for _, table := range migrate.Tables {
			db.Exec(`SELECT setval(pg_get_serial_sequence('` + table + `', 'id'), 1, false)`)
		}
	}
	empty := func() {
		for i := len(migrate.Tables) - 1; i >= 0; i-- { // children before parents
			// A missing table is tolerated: a test may have dropped ADK's on
			// purpose, and clearing what is not there is what "empty" means.
			if _, err := db.Exec(`DELETE FROM ` + migrate.Tables[i]); err != nil &&
				!storage.IsMissingTable(err) {
				t.Fatalf("clear %s: %v", migrate.Tables[i], err)
			}
		}
		db.Exec(`DELETE FROM memory_fts`)
		resetSequences()
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

	exclusive(t, dsn)
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

// A column the source has and the target does not stops the copy, by name.
//
// Copying the rest and saying nothing is silent data loss on the one operation
// nobody re-runs to check: the source is about to stop being read, and the
// value in that column is then gone. The two schema definitions being one
// commit apart is a real situation, and the answer is to say so — the fix is
// usually one line in migrations/postgres/0001_init.sql.
func TestColumnOnlyInTheSourceStopsTheCopy(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN")
	}
	ctx := context.Background()

	exclusive(t, dsn)
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

	_, err = migrate.Run(ctx, src, dst, migrate.Options{})
	var dropped *migrate.DroppedColumnsError
	if !errors.As(err, &dropped) {
		t.Fatalf("多出来的列被静默丢掉了: %v", err)
	}
	if dropped.Table != "task_runs" || !slices.Contains(dropped.Columns, "only_here") {
		t.Errorf("报告的不是那一列: %+v", dropped)
	}
	if !strings.Contains(dropped.Error(), "0001_init.sql") {
		t.Errorf("错误没告诉人怎么修: %s", dropped)
	}

	// And it can be overridden, for somebody who has looked at the list.
	rep, err := migrate.Run(ctx, src, dst, migrate.Options{AllowDroppingColumns: true})
	if err != nil {
		t.Fatalf("显式放行之后还是失败: %v", err)
	}
	if rep.Copied["task_runs"] != 1 {
		t.Errorf("task_runs 复制了 %d 行，want 1", rep.Copied["task_runs"])
	}
}

// A dry run is asked before the target is ready — that is what it is for.
//
// It used to refuse: the first table it reached was one of ADK's, which the
// migration file does not create, so it errored out before reporting a single
// count. An operator deciding whether to migrate got nothing.
func TestDryRunWorksBeforeTheTargetHasADKsTables(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN")
	}
	exclusive(t, dsn)
	ctx := context.Background()

	srcPath := filepath.Join(t.TempDir(), "state.db")
	seedSQLite(t, srcPath)
	src, err := storage.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	dst := freshPostgres(t, dsn)
	dropADKTables(t, dst)
	// Put them back for whatever runs next: they belong to ADK's AutoMigrate,
	// and a package that leaves the shared database missing them makes the
	// next one fail for a reason that has nothing to do with it.
	t.Cleanup(func() {
		if _, _, err := jellysession.New(dsn); err != nil {
			t.Logf("恢复 ADK 的表失败: %v", err)
		}
	})

	rep, err := migrate.Run(ctx, src, dst, migrate.Options{DryRun: true})
	if err != nil {
		t.Fatalf("目标还没准备好时 dry-run 就失败了: %v", err)
	}
	if rep.Copied["sessions"] == 0 {
		t.Error("dry-run 没有报出 sessions 会搬多少")
	}
	if rep.Copied["tool_results"] == 0 {
		t.Error("dry-run 没有报出 tool_results 会搬多少")
	}
	// And it wrote nothing, including the tables AutoMigrate would create.
	var n int
	if err := dst.QueryRow(`SELECT count(*) FROM tool_results`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("dry-run 写进了 %d 行", n)
	}
}

// A column the target lacks but the source never filled is not data loss.
//
// tool_results carries a vestigial `label` from a schema nobody writes any
// more — every row has the empty string in it. Refusing to migrate over it
// would stop every real upgrade for no reason, and it did: the first
// end-to-end run of this migration against a real database failed on it.
func TestAnEmptyColumnTheTargetLacksDoesNotStopTheCopy(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN")
	}
	exclusive(t, dsn)
	ctx := context.Background()

	srcPath := filepath.Join(t.TempDir(), "state.db")
	seedSQLite(t, srcPath)
	src, err := storage.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(
		`ALTER TABLE task_runs ADD COLUMN vestigial TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	dst := freshPostgres(t, dsn)

	var notes []string
	rep, err := migrate.Run(ctx, src, dst, migrate.Options{
		Notes: func(s string) { notes = append(notes, s) },
	})
	if err != nil {
		t.Fatalf("一个全空的列就让迁移停住了: %v", err)
	}
	if rep.Copied["task_runs"] != 1 {
		t.Errorf("task_runs 复制了 %d 行，want 1", rep.Copied["task_runs"])
	}
	// Said out loud, because a schema difference is worth knowing about even
	// when nothing is lost by it.
	var mentioned bool
	for _, n := range notes {
		if strings.Contains(n, "vestigial") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("跳过的空列没有被说出来: %v", notes)
	}
}

// A column with real content still stops it.
func TestAColumnWithValuesTheTargetLacksStillStopsTheCopy(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN")
	}
	exclusive(t, dsn)
	ctx := context.Background()

	srcPath := filepath.Join(t.TempDir(), "state.db")
	seedSQLite(t, srcPath)
	src, err := storage.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(
		`ALTER TABLE task_runs ADD COLUMN only_here TEXT NOT NULL DEFAULT 'something'`); err != nil {
		t.Fatal(err)
	}
	dst := freshPostgres(t, dsn)

	_, err = migrate.Run(ctx, src, dst, migrate.Options{})
	var dropped *migrate.DroppedColumnsError
	if !errors.As(err, &dropped) {
		t.Fatalf("有值的列被静默丢掉了: %v", err)
	}
	if !slices.Contains(dropped.Columns, "only_here") {
		t.Errorf("报告的不是那一列: %+v", dropped)
	}
}

// dropADKTables removes the tables ADK creates, leaving the ones the migration
// file defines — the state an operator is in after running the migration and
// before starting the service.
func dropADKTables(t *testing.T, db *storage.DB) {
	t.Helper()
	for _, tbl := range []string{"events", "sessions", "app_states", "user_states"} {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + tbl + ` CASCADE`); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
}

// The service has to be able to write after a migration.
//
// Rows arrive with their ids, so PostgreSQL's identity sequences never
// advance — and the first row written afterwards asks for id 1, which the
// migration already used. Demonstrated before the fix: three rows copied,
// then a plain insert fails with duplicate key on the primary key. The
// migration reports success and the deployment cannot write.
func TestWritingWorksAfterAMigration(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN")
	}
	exclusive(t, dsn)
	ctx := context.Background()

	srcPath := filepath.Join(t.TempDir(), "state.db")
	seedSQLite(t, srcPath)
	src, err := storage.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := freshPostgres(t, dsn)

	if _, err := migrate.Run(ctx, src, dst, migrate.Options{}); err != nil {
		t.Fatal(err)
	}

	// Every table with a generated id, written through the store that owns
	// it — which is what a running service does the moment it comes up.
	rec, err := metrics.NewRecorder(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	if err := rec.Record(metrics.ToolCall{
		SessionID: sess, InvocationID: "after", CallID: "c1", Tool: "query_range",
		OK: true, At: time.Now(),
	}); err != nil {
		t.Errorf("迁移后写不进 tool_calls: %v", err)
	}

	db := dst
	if err := schedule.Record(db, "after-migration", time.Now(),
		"succeeded", "", "", sess, "after"); err != nil {
		t.Errorf("迁移后写不进 schedule_runs: %v", err)
	}
	if err := toolreg.SaveDecl(ctx, db, toolreg.Decl{
		Name: "after_migration", Suites: &[]string{"x"},
	}, "tester"); err != nil {
		t.Errorf("迁移后写不进 tool_decls/tool_decl_log: %v", err)
	}
}

// emptiedSQLite builds a SQLite database with every table the migration
// touches and nothing in them.
//
// Built by seeding and clearing rather than by running DDL here: the schema is
// whatever the stores create on open, and a copy of it in this file would be a
// fourth definition that drifts.
func emptiedSQLite(t *testing.T, path string) *storage.DB {
	t.Helper()
	seedSQLite(t, path)
	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for i := len(migrate.Tables) - 1; i >= 0; i-- {
		if _, err := db.Exec(`DELETE FROM ` + migrate.Tables[i]); err != nil &&
			!storage.IsMissingTable(err) {
			t.Fatalf("clear %s: %v", migrate.Tables[i], err)
		}
	}
	return db
}

// A row the target already holds under the same primary key, saying something
// different, has to stop the migration.
//
// This is the case the row-count check is blind to by construction: ON
// CONFLICT DO NOTHING keeps the row that is already there, so the counts come
// out equal and the run reports success while the target disagrees with the
// source about what that row says.
func TestARowTheTargetAlreadyHasWithOtherContentStopsTheCopy(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "src.db")
	seedSQLite(t, srcPath)
	src, err := storage.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := emptiedSQLite(t, filepath.Join(t.TempDir(), "dst.db"))

	if _, err := migrate.Run(ctx, src, dst, migrate.Options{}); err != nil {
		t.Fatal(err)
	}
	// Somebody else's row under a key this migration also carries.
	if _, err := dst.Exec(`UPDATE tool_calls SET tool = ?`, "别人的工具"); err != nil {
		t.Fatal(err)
	}

	_, err = migrate.Run(ctx, src, dst, migrate.Options{})
	var ce *migrate.ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("目标库的行和源库不一样，迁移却说成功了: %v", err)
	}
	if ce.Table != "tool_calls" || ce.Total != 1 || len(ce.Conflicts) != 1 {
		t.Fatalf("报错没说清是哪张表哪几行: %+v", ce)
	}
	if c := ce.Conflicts[0]; c.Column != "tool" || c.Target != "别人的工具" || c.Source != "query_range" {
		t.Errorf("报错没说清是哪一列、两边各是什么: %+v", c)
	}
	if !strings.Contains(ce.Error(), "别人的工具") {
		t.Errorf("错误文本里看不到冲突的值: %s", ce.Error())
	}
}

// …and an unchanged re-run must not report one. A comparison that cries wolf
// on a row it copied itself is worse than no comparison: it turns "run it
// again" into a dead end.
func TestReRunningOverIdenticalRowsReportsNoConflict(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "src.db")
	seedSQLite(t, srcPath)
	src, err := storage.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := emptiedSQLite(t, filepath.Join(t.TempDir(), "dst.db"))

	first, err := migrate.Run(ctx, src, dst, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := migrate.Run(ctx, src, dst, migrate.Options{})
	if err != nil {
		t.Fatalf("重跑报了冲突: %v", err)
	}
	for _, table := range first.Order {
		if first.Copied[table] == 0 {
			continue
		}
		if second.Copied[table] != 0 {
			t.Errorf("%s 第二遍又复制了 %d 行", table, second.Copied[table])
		}
		if second.Skipped[table] != first.Copied[table] {
			t.Errorf("%s 第一遍搬了 %d 行，第二遍只认出 %d 行已存在",
				table, first.Copied[table], second.Skipped[table])
		}
	}
}

// The same check against a real PostgreSQL, which is the half that can go
// wrong in the other direction.
//
// SQLite keeps a timestamp as RFC3339 text and PostgreSQL hands the same
// column back as a time.Time, so a comparison that does not fold the two
// spellings together calls every copied row a conflict — and a migration that
// refuses to resume is a worse bug than one that skips silently. Both
// directions are asserted here: identical rows are quiet, a changed one is
// not.
func TestConflictDetectionOnPostgres(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN")
	}
	ctx := context.Background()

	exclusive(t, dsn)
	srcPath := filepath.Join(t.TempDir(), "state.db")
	seedSQLite(t, srcPath)
	src, err := storage.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := freshPostgres(t, dsn)

	if _, err := migrate.Run(ctx, src, dst, migrate.Options{}); err != nil {
		t.Fatal(err)
	}
	// Quiet on a resume: every row is already there and identical.
	if _, err := migrate.Run(ctx, src, dst, migrate.Options{}); err != nil {
		t.Fatalf("原样重跑被当成冲突了 —— 大概率是两种方言的同一个值没有归一化: %v", err)
	}

	// Loud on a row that differs. tool_results.payload is bytes on both
	// sides, tool_calls.tool is text, and events.content is the JSON blob the
	// session store writes — one of each, so a normalisation that only works
	// for strings does not pass.
	for _, tc := range []struct{ table, set, col, want string }{
		{"tool_calls", `UPDATE tool_calls SET tool = '别人的工具'`, "tool", "别人的工具"},
		{"tool_results", `UPDATE tool_results SET payload = '别人的产物'`, "payload", "别人的产物"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			var before string
			if err := dst.QueryRow(`SELECT ` + tc.col + ` FROM ` + tc.table).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := dst.Exec(tc.set); err != nil {
				t.Fatal(err)
			}
			defer dst.Exec(`UPDATE `+tc.table+` SET `+tc.col+` = ?`, before)

			_, err := migrate.Run(ctx, src, dst, migrate.Options{})
			var ce *migrate.ConflictError
			if !errors.As(err, &ce) {
				t.Fatalf("目标库的 %s 和源库不一样，迁移却说成功了: %v", tc.table, err)
			}
			if ce.Table != tc.table {
				t.Fatalf("报的是 %s，改的是 %s", ce.Table, tc.table)
			}
			if c := ce.Conflicts[0]; c.Column != tc.col || c.Target != tc.want {
				t.Errorf("报错没说清是哪一列、目标库里是什么: %+v", c)
			}
		})
	}
}
