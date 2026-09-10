package toolreg

import (
	"context"
	"testing"
	"time"
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

	src := NewDBSource(db).WithPoll(10 * time.Millisecond)
	ch := src.WatchFrom(ctx, 0) // baseline behind the change, so every tick sees it

	// Several ticks all fail to load.
	select {
	case metas := <-ch:
		t.Fatalf("读不出来，居然还报了一次变更: %+v", metas)
	case <-time.After(150 * time.Millisecond):
	}

	// The row is repaired, and the change is still owed to this watcher. The
	// log did not move — nothing new happened — so the only thing that can
	// deliver it is a baseline that never advanced past the failure.
	if _, err := db.Exec(`UPDATE tool_decls SET use_cases = NULL`); err != nil {
		t.Fatal(err)
	}

	select {
	case metas := <-ch:
		if len(metas) != 1 || metas[0].Name != "query_range" {
			t.Fatalf("报上来的内容不对: %+v", metas)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("加载失败过一次，这次变更就再也没被报上来 —— 版本号在加载之前就前进了")
	}
}
