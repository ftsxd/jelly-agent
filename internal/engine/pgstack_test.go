package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/memory"
	jellymetrics "github.com/jelly-agent/jelly-agent/internal/metrics"
	"github.com/jelly-agent/jelly-agent/internal/record"
	"github.com/jelly-agent/jelly-agent/internal/schedule"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	"github.com/jelly-agent/jelly-agent/internal/task"
	adksession "google.golang.org/adk/session"
)

// End to end over every store, against a real PostgreSQL. Opt-in.
//
// The unit tests all run on SQLite, where `?` is the native placeholder and
// the stores create their own schema — so a query that was never rebound and a
// table that only exists because a store invented it both pass there. This is
// the only test that can tell.
func TestEveryStoreWorksAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN to exercise the stores against PostgreSQL")
	}
	exclusive(t, dsn)
	ctx := context.Background()
	e := New(&config.Config{Storage: config.Storage{DSN: dsn}})
	t.Cleanup(e.Close)

	if got := e.StateRef(); got != dsn {
		t.Fatalf("the engine did not take the configured DSN: %q", got)
	}
	db, err := e.StateDB()
	if err != nil {
		t.Fatalf("StateDB: %v", err)
	}

	const sess = "pgstack-session"
	t.Cleanup(func() {
		db.Exec(`DELETE FROM task_runs WHERE session_id = ?`, sess)
		db.Exec(`DELETE FROM schedule_runs WHERE task = ?`, "pgstack")
		db.Exec(`DELETE FROM memory_fts WHERE session_id = ?`, sess)
	})

	t.Run("ADK 的会话服务能建表并写读", func(t *testing.T) {
		svc, err := jellysession.New(dsn)
		if err != nil {
			t.Fatalf("session service: %v", err)
		}
		if _, err := svc.Create(ctx, &adksession.CreateRequest{
			AppName: AppName, UserID: UserID, SessionID: sess,
		}); err != nil {
			t.Fatalf("create session: %v", err)
		}
		t.Cleanup(func() {
			svc.Delete(ctx, &adksession.DeleteRequest{
				AppName: AppName, UserID: UserID, SessionID: sess,
			})
		})
		metas, total, err := jellysession.ListPage(db, AppName, UserID, 10, 0)
		if err != nil {
			t.Fatalf("ListPage: %v", err)
		}
		if total == 0 || len(metas) == 0 {
			t.Fatal("会话写进去了但列不出来")
		}
	})

	t.Run("工具产物：句柄分配、读回、分段", func(t *testing.T) {
		store, err := record.Open(dsn)
		if err != nil {
			t.Fatalf("record.Open: %v", err)
		}
		// Registered in this order on purpose: cleanups run last-in-first-out,
		// so the delete happens before the handle closes. A `defer
		// store.Close()` would run at function exit — before any cleanup — and
		// the delete would then land on a closed store, fail silently, and
		// leave the row behind for the next run to trip over.
		t.Cleanup(func() { store.Close() })
		sc := record.Scope{AppName: AppName, UserID: UserID, SessionID: sess}
		t.Cleanup(func() {
			if err := store.Delete(ctx, sc); err != nil {
				t.Errorf("清理失败，残留会污染下一次运行: %v", err)
			}
		})

		payload := []byte("第一行\n第二行\n第三行\n")
		sent := time.Now()
		label, err := store.Put(ctx, record.Record{
			Scope: sc, InvocationID: "inv1", CallID: "c1",
			Tool: "query_range", At: sent, Payload: payload,
		})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if label != "e1" {
			t.Errorf("handle = %q, want e1", label)
		}
		chunk, err := store.ReadLabel(ctx, sc, label, 0, 9)
		if err != nil {
			t.Fatalf("ReadLabel: %v", err)
		}
		if len(chunk.Data) == 0 || len(chunk.Data) > 9 {
			t.Errorf("windowed read returned %d bytes", len(chunk.Data))
		}

		// The listing carries the stored timestamp back, so it has to survive
		// the round trip. Asserted because the first version of this test did
		// not, and a column declared as a real timestamp passed anyway — the
		// value came back in the database's own format instead of the one the
		// code writes, and nothing looked at it.
		items, err := store.List(ctx, sc, record.ListOpts{InvocationID: "inv1"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("List returned %d items", len(items))
		}
		if items[0].At.IsZero() {
			t.Error("stored timestamp came back as the zero time")
		}
		if d := items[0].At.Sub(sent); d > time.Second || d < -time.Second {
			t.Errorf("timestamp drifted by %s on the round trip", d)
		}
	})

	t.Run("保留期清理：正文清空、句柄仍在", func(t *testing.T) {
		// The sweep runs at startup and every hour. On PostgreSQL it used to
		// fail outright — the statement assigned SQLite's blob literal X'' to
		// a bytea — so nothing ever expired, silently, on the one path whose
		// whole job is to stop the database growing.
		store, err := record.Open(dsn)
		if err != nil {
			t.Fatalf("record.Open: %v", err)
		}
		t.Cleanup(func() { store.Close() })
		sc := record.Scope{AppName: AppName, UserID: UserID, SessionID: sess + "-exp"}
		t.Cleanup(func() {
			if err := store.Delete(ctx, sc); err != nil {
				t.Errorf("清理失败，残留会污染下一次运行: %v", err)
			}
		})

		label, err := store.Put(ctx, record.Record{
			Scope: sc, InvocationID: "inv1", CallID: "c1",
			Tool: "query_logs", At: time.Now().Add(-48 * time.Hour),
			Payload: []byte("很长的日志正文"),
		})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}

		n, err := store.Expire(ctx, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("Expire: %v", err)
		}
		if n != 1 {
			t.Fatalf("expired %d rows, want 1", n)
		}

		// The row stays so the handle keeps resolving, and says why.
		_, err = store.ReadLabel(ctx, sc, label, 0, 100)
		if !errors.Is(err, record.ErrExpired) {
			t.Errorf("读过期句柄 err = %v, want ErrExpired", err)
		}
	})

	t.Run("任务归属", func(t *testing.T) {
		id := task.ID(sess, "inv1")
		if err := task.Link(db, id, sess, "inv1"); err != nil {
			t.Fatalf("Link: %v", err)
		}
		got, err := task.OfSession(db, sess)
		if err != nil || got["inv1"] != id {
			t.Fatalf("OfSession = %v, %v", got, err)
		}
	})

	t.Run("排程运行记录", func(t *testing.T) {
		if err := schedule.Record(db, "pgstack", time.Now().Add(-time.Minute),
			"succeeded", "out", "", sess, "inv1"); err != nil {
			t.Fatalf("Record: %v", err)
		}
		runs, total, err := schedule.List(db, "pgstack", 10, 0)
		if err != nil || total != 1 || len(runs) != 1 {
			t.Fatalf("List = %d rows, total %d, %v", len(runs), total, err)
		}
		if runs[0].SessionID != sess {
			t.Errorf("run 的 session id = %q", runs[0].SessionID)
		}
	})

	t.Run("记忆索引的清理路径", func(t *testing.T) {
		if _, err := memory.PurgeSessions(db, []string{sess}); err != nil {
			t.Fatalf("PurgeSessions: %v", err)
		}
		if _, err := memory.PurgeOrphanIndex(db); err != nil {
			t.Fatalf("PurgeOrphanIndex: %v", err)
		}
	})

	t.Run("调用记录：写、读、删", func(t *testing.T) {
		// Asserting a write and a read, not just a delete. A delete against an
		// empty table succeeds on any dialect, so the first version of this
		// subtest would have passed with the booleans still written as
		// integers — which is exactly the bug PostgreSQL rejects.
		tr := e.Metrics()
		if tr == nil {
			t.Skip("metrics 未启用")
		}
		rec := tr.Recorder()
		if rec == nil {
			t.Fatal("tracker 没有 recorder —— 埋点被静默关掉了")
		}
		if err := rec.Record(jellymetrics.ToolCall{
			SessionID: sess, InvocationID: "inv1", CallID: "c1",
			Tool: "query_range", Agent: "root", Duration: 12 * time.Millisecond,
			OK: true, At: time.Now(),
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		// In this database, not merely somewhere. A recorder that fell back to
		// the default path would write to a local SQLite file and read it back
		// again, so a round-trip assertion alone cannot tell — which is how
		// the metrics store came to be pointed at a different database from
		// every table it joins against.
		var landed int
		if err := db.QueryRow(
			`SELECT count(*) FROM tool_calls WHERE session_id = ?`, sess).Scan(&landed); err != nil {
			t.Fatal(err)
		}
		if landed != 1 {
			t.Fatalf("PostgreSQL 里有 %d 行 tool_calls，说明埋点写到别的库去了", landed)
		}

		rows, err := tr.ByInvocation(sess, "inv1")
		if err != nil {
			t.Fatalf("ByInvocation: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("read back %d rows, want 1", len(rows))
		}
		if !rows[0].OK || rows[0].Tool != "query_range" || rows[0].DurationMS != 12 {
			t.Errorf("row = %+v", rows[0])
		}

		n, err := tr.DeleteSessions([]string{sess})
		if err != nil {
			t.Fatalf("DeleteSessions: %v", err)
		}
		if n != 1 {
			t.Errorf("deleted %d rows, want 1", n)
		}
	})
}

// Close has to release the delivery store too.
//
// It released the metrics recorder and the state handle and left this one
// open, so every config reload abandoned the pool the previous engine had
// created — free against a SQLite file, and on PostgreSQL a slow walk into
// "too many clients already".
//
// Runs on SQLite, because what is being asserted is that Close reached the
// store at all; the consequence is worse on PostgreSQL but the bug is the same
// one on both.
func TestCloseReleasesTheDeliveryStore(t *testing.T) {
	e := New(&config.Config{Storage: config.Storage{
		DSN: filepath.Join(t.TempDir(), "state.db"),
	}})
	store, err := e.Records()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background(), record.Scope{
		AppName: AppName, UserID: UserID, SessionID: "s1",
	}, record.ListOpts{}); err != nil {
		t.Fatalf("store is not usable before Close: %v", err)
	}

	e.Close()

	// A closed handle refuses; an open one answers. That is the difference
	// between releasing the pool and leaking it.
	_, err = store.List(context.Background(), record.Scope{
		AppName: AppName, UserID: UserID, SessionID: "s1",
	}, record.ListOpts{})
	if err == nil {
		t.Error("Close 之后产物存储仍然可用 —— 它的连接池被泄漏了")
	}
}

// Close must not open what it is closing.
//
// Reaching for the store through records() rather than the field would create
// a connection pool in order to release it, which on a reload is exactly the
// leak this is meant to prevent — one per reload, none of them ever used.
func TestCloseDoesNotOpenTheDeliveryStore(t *testing.T) {
	// A path no database can be created at, so an open would fail loudly.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := New(&config.Config{Storage: config.Storage{
		DSN: filepath.Join(blocker, "state.db"),
	}})
	e.Close() // must not panic, must not try to open

	if e.recordStore != nil {
		t.Error("Close 把产物存储打开了")
	}
}
