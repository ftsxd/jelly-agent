package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/memory"
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
		db.Exec(`DELETE FROM memory_index WHERE session_id = ?`, sess)
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
		defer store.Close()
		sc := record.Scope{AppName: AppName, UserID: UserID, SessionID: sess}
		t.Cleanup(func() { store.Delete(ctx, sc) })

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

	t.Run("调用记录", func(t *testing.T) {
		tr := e.Metrics()
		if tr == nil {
			t.Skip("metrics 未启用")
		}
		if _, err := tr.DeleteSessions([]string{sess}); err != nil {
			t.Fatalf("DeleteSessions: %v", err)
		}
	})
}
