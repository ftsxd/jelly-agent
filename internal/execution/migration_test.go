package execution

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// The incremental DDL intentionally uses the SQL shared by both databases.
// Keep it aligned with runtime schemas and ensure reruns preserve old rows.
// Actual PostgreSQL execution is verified separately in an isolated fixture.
func TestExecutionUpgradePreservesExistingRecords(t *testing.T) {
	ddl, err := os.ReadFile(filepath.Join("..", "..", "migrations", "postgres", "0003_execution_runtime_upgrade.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range []string{"none", "approvals", "runs", "both"} {
		t.Run(existing, func(t *testing.T) {
			db, err := storage.Open(filepath.Join(t.TempDir(), "old.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.Exec(`CREATE TABLE old_chat (message TEXT NOT NULL); INSERT INTO old_chat VALUES ('keep chat');`); err != nil {
				t.Fatal(err)
			}
			if existing == "approvals" || existing == "both" {
				if err = EnsureApprovalSchema(db); err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`INSERT INTO execution_approvals(id,session_id,agent,invocation_id,call_id,request_json,config_hash,origin_json,reason,state,created_ms,expires_ms,outcome) VALUES('keep-approval','s','ops','i','c','{}','hash','{}','reason','consumed',1,2,'unknown')`)
				if err != nil {
					t.Fatal(err)
				}
			}
			if existing == "runs" || existing == "both" {
				if err = EnsureJournalSchema(db); err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`INSERT INTO execution_runs(exec_id,session_id,agent,profile,approval_id,workspace_root,scope,owner,daemon,state,created_ms,deadline_ms) VALUES('keep-run','s','ops','p','','/private-fixture','scope','owner','daemon','unknown',1,2)`)
				if err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if _, err = db.Exec(string(ddl)); err != nil {
					t.Fatal(err)
				}
			}
			var got string
			if err = db.QueryRow(`SELECT message FROM old_chat`).Scan(&got); err != nil || got != "keep chat" {
				t.Fatal("chat changed", got, err)
			}
			if existing == "approvals" || existing == "both" {
				if err = db.QueryRow(`SELECT outcome FROM execution_approvals WHERE id='keep-approval'`).Scan(&got); err != nil || got != "unknown" {
					t.Fatal("approval changed", got, err)
				}
			}
			if existing == "runs" || existing == "both" {
				if err = db.QueryRow(`SELECT state FROM execution_runs WHERE exec_id='keep-run'`).Scan(&got); err != nil || got != "unknown" {
					t.Fatal("run changed", got, err)
				}
			}
			want, _ := journalStore(t)
			for _, table := range []string{"execution_approvals", "execution_runs"} {
				a, err := storage.Columns(db, table)
				if err != nil {
					t.Fatal(err)
				}
				b, err := storage.Columns(want.DB, table)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(a, b) {
					t.Fatal("upgrade/schema drift", table, a, b)
				}
			}
		})
	}
}
