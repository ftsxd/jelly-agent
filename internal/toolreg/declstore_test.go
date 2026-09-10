package toolreg

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// declDB opens a scratch database with the declaration tables.
//
// Through EnsureSchema rather than its own DDL: this package's schema is
// already defined twice (that constant and the PostgreSQL migration), and a
// copy in the test would be a third that nothing compares.
func declDB(t *testing.T) *storage.DB {
	t.Helper()
	ref := os.Getenv("JELLY_PG_DSN")
	if ref == "" {
		ref = filepath.Join(t.TempDir(), "state.db")
	} else {
		exclusive(t, ref)
	}
	db, err := storage.Open(ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	if db.Kind() == storage.KindPostgres {
		// PostgreSQL's schema is the migration file; a test that recreated it
		// would be testing its own copy.
		for _, tbl := range []string{"tool_decl_log", "tool_decls"} {
			if _, err := db.Exec(`DELETE FROM ` + tbl); err != nil {
				t.Fatalf("clear %s: %v", tbl, err)
			}
		}
		t.Cleanup(func() {
			db.Exec(`DELETE FROM tool_decl_log`)
			db.Exec(`DELETE FROM tool_decls`)
		})
	}
	return db
}

func str(s string) *string       { return &s }
func list(v ...string) *[]string { return &v }

// A save carries the fields somebody changed and must leave the others alone.
//
// This is the bug the table exists for. When this layer was a YAML file the
// console rewrote in full, a save that set one field wrote back its idea of
// every other one — and on 2026-09-08 that cleared a suites list nobody had
// touched, with no history to find out what it had been.
func TestSavingOneFieldLeavesTheOthersAlone(t *testing.T) {
	ctx := context.Background()
	db := declDB(t)

	if err := SaveDecl(ctx, db, Decl{
		Server: "n9e-mcp", Name: "query_range",
		Suites: list("promql"), Produces: str("metric_series"),
	}, "alice"); err != nil {
		t.Fatal(err)
	}
	// A second save that says nothing about suites.
	if err := SaveDecl(ctx, db, Decl{
		Server: "n9e-mcp", Name: "query_range",
		Description: str("查询一段时间的指标"),
	}, "bob"); err != nil {
		t.Fatal(err)
	}

	got := loadOne(t, db, "n9e-mcp", "query_range")
	if !slices.Equal(got.Suites, []string{"promql"}) {
		t.Errorf("suites = %v —— 另一个字段的保存把它冲掉了", got.Suites)
	}
	if got.Description != "查询一段时间的指标" || got.Produces != "metric_series" {
		t.Errorf("decl = %+v", got)
	}
}

// Clearing is a statement, and a different one from saying nothing.
func TestClearingAFieldIsNotTheSameAsOmittingIt(t *testing.T) {
	ctx := context.Background()
	db := declDB(t)
	if err := SaveDecl(ctx, db, Decl{
		Server: "n9e-mcp", Name: "query_logs",
		Suites: list("logs"), Description: str("查日志"),
	}, ""); err != nil {
		t.Fatal(err)
	}
	if err := SaveDecl(ctx, db, Decl{
		Server: "n9e-mcp", Name: "query_logs", Suites: list(),
	}, ""); err != nil {
		t.Fatal(err)
	}
	got := loadOne(t, db, "n9e-mcp", "query_logs")
	if len(got.Suites) != 0 {
		t.Errorf("显式清空之后 suites 还是 %v", got.Suites)
	}
	if got.Description != "查日志" {
		t.Errorf("清空一个字段把另一个也带走了: %q", got.Description)
	}
}

// A declaration with nothing left in it is removed, not kept as a row of
// NULLs: "not declared" and "declared as nothing" look the same downstream and
// only the first is true.
func TestADeclarationEmptiedCompletelyIsRemoved(t *testing.T) {
	ctx := context.Background()
	db := declDB(t)
	if err := SaveDecl(ctx, db, Decl{Name: "web_search", Suites: list("research")}, ""); err != nil {
		t.Fatal(err)
	}
	if err := SaveDecl(ctx, db, Decl{Name: "web_search", Suites: list()}, ""); err != nil {
		t.Fatal(err)
	}
	metas, err := NewDBSource(db).Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metas {
		if m.Name == "web_search" {
			t.Errorf("清空之后这条声明还在: %+v", m)
		}
	}
}

// Every change is recorded, which is what was missing.
func TestEveryChangeIsRecordedWithWhoAndWhat(t *testing.T) {
	ctx := context.Background()
	db := declDB(t)
	for _, s := range []struct {
		by     string
		suites []string
	}{{"alice", []string{"promql"}}, {"bob", []string{"promql", "metrics"}}} {
		if err := SaveDecl(ctx, db, Decl{
			Server: "n9e-mcp", Name: "query_instant", Suites: &s.suites,
		}, s.by); err != nil {
			t.Fatal(err)
		}
	}

	hist, err := History(ctx, db, "n9e-mcp", "query_instant", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("记了 %d 条，want 2", len(hist))
	}
	// Newest first.
	if hist[0].ChangedBy != "bob" || hist[1].ChangedBy != "alice" {
		t.Errorf("顺序或署名不对: %+v", hist)
	}
	// And the snapshot answers "what was it at the time" without a replay.
	if hist[1].Snapshot == nil || !slices.Equal(hist[1].Snapshot.Suites, []string{"promql"}) {
		t.Errorf("最早那次的快照没记住当时的值: %+v", hist[1].Snapshot)
	}
	if hist[0].ChangedAt.IsZero() {
		t.Error("没有记录时间")
	}
}

// The log is also the version every process polls, so a change cannot be
// applied without being announced.
func TestTheLogIsTheVersion(t *testing.T) {
	ctx := context.Background()
	db := declDB(t)
	src := NewDBSource(db)
	before := src.version(ctx)
	if err := SaveDecl(ctx, db, Decl{Name: "fetch_url", Suites: list("web")}, ""); err != nil {
		t.Fatal(err)
	}
	if after := src.version(ctx); after <= before {
		t.Errorf("保存之后版本号没有前进：%d → %d", before, after)
	}
}

func loadOne(t *testing.T, db *storage.DB, server, name string) ops.ToolMetadata {
	t.Helper()
	metas, err := NewDBSource(db).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metas {
		if m.Server == server && m.Name == name {
			return m
		}
	}
	t.Fatalf("%s/%s 不在声明里", server, name)
	return ops.ToolMetadata{}
}

// The database layer patches the file layer rather than competing with it.
//
// Both declare the same tool, and without the overlay distinction that is a
// registry conflict resolved by dropping one of them — so a console edit to
// one field would discard everything a hand-written file said about the rest.
func TestTheDatabaseLayerPatchesTheFileLayer(t *testing.T) {
	ctx := context.Background()
	db := declDB(t)

	base := StaticSource{Label: "file", Metas: []ops.ToolMetadata{{
		Server: "n9e-mcp", Name: "query_range",
		Description: "文件里写的描述",
		Produces:    "metric_series",
		SideEffect:  "read_only",
	}}}
	// The console sets one field and says nothing about the others.
	if err := SaveDecl(ctx, db, Decl{
		Server: "n9e-mcp", Name: "query_range", Suites: list("promql"),
	}, "alice"); err != nil {
		t.Fatal(err)
	}

	metas, err := Merge(ctx, base, NewDBSource(db))
	if err != nil {
		t.Fatal(err)
	}
	var got *ops.ToolMetadata
	for i := range metas {
		if metas[i].Name == "query_range" {
			got = &metas[i]
		}
	}
	if got == nil {
		t.Fatalf("合并之后这个工具不见了: %+v", metas)
	}
	if len(metas) != 1 {
		t.Errorf("合并出 %d 条 —— overlay 被当成了另一个声明而不是补丁", len(metas))
	}
	if !slices.Equal(got.Suites, []string{"promql"}) {
		t.Errorf("控制台设的 suites 没生效: %v", got.Suites)
	}
	if got.Description != "文件里写的描述" || got.Produces != "metric_series" || got.SideEffect != "read_only" {
		t.Errorf("控制台只改了一个字段，却把文件里的其余部分冲掉了: %+v", got)
	}
}

// A deployment upgrading across this change has a console.yaml holding
// decisions somebody made, and they must survive.
func TestImportingAnExistingConsoleFile(t *testing.T) {
	ctx := context.Background()
	db := declDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ConsoleFile)
	if err := os.WriteFile(path, []byte(`tools:
  - name: query_range
    server: n9e-mcp
    produces: metric_series
    side_effect: read_only
    suites: [promql]
  - name: query_logs
    server: n9e-mcp
    description: ""
    suites: [logs]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := ImportConsoleFile(ctx, db, path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("导入了 %d 条，want 2", n)
	}

	got := loadOne(t, db, "n9e-mcp", "query_range")
	if !slices.Equal(got.Suites, []string{"promql"}) ||
		got.Produces != "metric_series" || got.SideEffect != "read_only" {
		t.Errorf("导入的内容不对: %+v", got)
	}

	// A field the file left blank must not become an explicit override. A
	// YAML zero value meant "not declared", and carrying it in as an empty
	// string would turn every unset field into one — the exact confusion the
	// table exists to remove.
	logs := loadOne(t, db, "n9e-mcp", "query_logs")
	if logs.Description != "" {
		t.Errorf("空描述被当成了覆盖: %q", logs.Description)
	}
	hist, err := History(ctx, db, "n9e-mcp", "query_logs", 5)
	if err != nil || len(hist) != 1 {
		t.Fatalf("history = %d, %v", len(hist), err)
	}
	if hist[0].Snapshot == nil || hist[0].Snapshot.Description != "" {
		t.Errorf("快照里带上了空描述: %+v", hist[0].Snapshot)
	}

	// Renamed, which is both the record and what stops the next start from
	// importing it again.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("导入之后原文件还在原地")
	}
	if _, err := os.Stat(path + ".imported"); err != nil {
		t.Errorf("原文件没有被保留下来: %v", err)
	}
}

// Importing runs once. A table with anything in it has already been decided
// on, and a second import would undo whatever was decided since.
func TestImportingSkipsATableThatIsNotEmpty(t *testing.T) {
	ctx := context.Background()
	db := declDB(t)
	if err := SaveDecl(ctx, db, Decl{Name: "web_search", Suites: list("research")}, "alice"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ConsoleFile)
	if err := os.WriteFile(path, []byte("tools:\n  - name: query_range\n    suites: [promql]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := ImportConsoleFile(ctx, db, path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("表非空却导入了 %d 条", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("没有导入，却把文件改名了")
	}
}

// A fresh deployment has no file, which is the common case and not an error.
func TestImportingWithNoFileIsFine(t *testing.T) {
	n, err := ImportConsoleFile(context.Background(), declDB(t),
		filepath.Join(t.TempDir(), ConsoleFile))
	if err != nil || n != 0 {
		t.Errorf("n = %d, err = %v", n, err)
	}
}

// The overlay patches the file layer, and it is the only overlay.
//
// This behaviour used to belong to console.yaml, applied inside
// FileSource.Load: whichever file sorted last did not win outright, its
// fields were folded into the others. That layer moved to the database, and
// FileSource stopped treating any filename specially — a second overlay would
// silently compete with what the console shows.
func TestTheOverlayFoldsIntoTheFileLayer(t *testing.T) {
	ctx := context.Background()
	db := declDB(t)

	hand := StaticSource{Label: "hand-written", Metas: []ops.ToolMetadata{{
		Name: "query_range", Server: "n9e",
		Description: "查询一段时间的指标曲线",
		Produces:    "log_excerpt",
		SideEffect:  "read_only",
		Aliases:     []string{"range_query"},
	}}}
	if err := SaveDecl(ctx, db, Decl{
		Server: "n9e", Name: "query_range", Produces: str("metric_series"),
	}, "alice"); err != nil {
		t.Fatal(err)
	}

	metas, err := Merge(ctx, hand, NewDBSource(db))
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("metas = %d, want the overlay folded into the file entry: %+v", len(metas), metas)
	}
	got := metas[0]
	if got.Produces != "metric_series" {
		t.Errorf("produces = %q, want the overlay's", got.Produces)
	}
	// Everything the overlay said nothing about survives.
	if got.Description != "查询一段时间的指标曲线" || got.SideEffect != "read_only" ||
		!slices.Equal(got.Aliases, []string{"range_query"}) {
		t.Errorf("覆盖一个字段把文件层的其余部分冲掉了: %+v", got)
	}
}
