package toolreg

import (
	"context"
	"testing"
)

// A load that fails must not spend the notification it was answering.
//
// The version was advanced before the load, so one transient failure — a
// database briefly unreachable, a schema being migrated — marked the change
// as seen and moved on. Nothing retried it: the only trigger is the version
// moving, and it had already moved. The edit was then invisible to this
// process until somebody happened to make another one.
func TestALoadThatFailsDoesNotSwallowTheChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := declDB(t)

	// The log is readable and has moved; the load fails. A column holding
	// valid JSON that is not an array is the shape of it — a half-written
	// row, a migration in flight — and it is held that way deliberately so
	// every tick in the window fails rather than one of them by luck.
	//
	// An object rather than a broken string: PostgreSQL's column is jsonb
	// and refuses anything that is not JSON at all, so the failure has to be
	// one both dialects can hold. Both then fail the same way, decoding it
	// into []string.
	if err := SaveDecl(ctx, db, Decl{
		Server: "n9e", Name: "query_range", Suites: list("promql"),
	}, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tool_decls SET use_cases = ?`, `{}`); err != nil {
		t.Fatal(err)
	}

	// A caller that keeps its own baseline: the loop every consumer runs.
	src := NewDBSource(db)
	seen := int64(0) // behind the change, so every poll sees it

	// Several polls all fail to load, and none of them may claim the change.
	for range 5 {
		metas, v, changed, err := src.Poll(ctx, seen)
		if err == nil {
			t.Fatalf("读不出来，Poll 居然成功了: %+v", metas)
		}
		if changed {
			t.Fatal("加载失败的一轮不能报成有变更")
		}
		if v != 0 {
			t.Fatalf("加载失败的一轮回了一个可以推进基线的版本号 %d", v)
		}
	}

	// The row is repaired, and the change is still owed. The log did not move
	// — nothing new happened — so the only thing that can deliver it is a
	// baseline that never advanced past the failure.
	if _, err := db.Exec(`UPDATE tool_decls SET use_cases = NULL`); err != nil {
		t.Fatal(err)
	}
	metas, v, changed, err := src.Poll(ctx, seen)
	if err != nil || !changed {
		t.Fatalf("加载失败过一次，这次变更就再也没被报上来: changed=%v err=%v", changed, err)
	}
	if v <= seen {
		t.Errorf("版本号没有前进: %d", v)
	}
	if len(metas) != 1 || metas[0].Name != "query_range" {
		t.Fatalf("报上来的内容不对: %+v", metas)
	}

	// And the baseline the caller now holds stops it repeating.
	if _, _, changed, err := src.Poll(ctx, v); err != nil || changed {
		t.Errorf("装好之后又报了一次: changed=%v err=%v", changed, err)
	}
}
