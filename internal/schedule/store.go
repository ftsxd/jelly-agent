// Package schedule persists what each cron task run did.
//
// A run row is the only record that a scheduled task ever executed. Everything
// else about the run — its steps, its tool calls, the results it stored — is
// keyed by (session_id, invocation_id) in three other tables, so a row without
// those two ids can never be joined to any of it. That is why they are here.
package schedule

import (
	"time"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// Run is one execution of a scheduled task.
type Run struct {
	ID         int64     `json:"id"`
	Task       string    `json:"task"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Status     string    `json:"status"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`

	// SessionID and InvocationID tie this row to the events, tool calls and
	// stored results the run produced. Without them a scheduled run shows in
	// the console as a status and a blob of text, with no way to reach the
	// steps behind it — every other table is keyed by exactly this pair.
	SessionID    string `json:"session_id,omitempty"`
	InvocationID string `json:"invocation_id,omitempty"`
}

const schema = `
CREATE TABLE IF NOT EXISTS schedule_runs (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	task          TEXT     NOT NULL,
	started_at    DATETIME NOT NULL,
	finished_at   DATETIME NOT NULL,
	status        TEXT     NOT NULL,
	output        TEXT,
	error         TEXT,
	session_id    TEXT     NOT NULL DEFAULT '',
	invocation_id TEXT     NOT NULL DEFAULT ''
)`

// migrate brings an existing table up to the current shape.
//
// CREATE TABLE IF NOT EXISTS does nothing to a table that already exists, so a
// column added later has to be added explicitly. This file used to re-run the
// create statement inline on every read and write, which looks like migration
// and is not: a deployment that had ever run the old binary would silently
// keep the old shape forever.
//
// Same idiom as internal/record.migrate and internal/metrics.addMissingColumns.
func migrate(db *storage.DB) error {
	return storage.EnsureColumns(db, "schedule_runs", []storage.Column{
		{Name: "session_id", DDL: "ALTER TABLE schedule_runs ADD COLUMN session_id TEXT NOT NULL DEFAULT ''"},
		{Name: "invocation_id", DDL: "ALTER TABLE schedule_runs ADD COLUMN invocation_id TEXT NOT NULL DEFAULT ''"},
	})
}

// Record writes one finished run.
//
// sessionID and invocationID may be empty — a run that failed before the agent
// started has neither, and an empty string is the honest answer rather than a
// fabricated id that would resolve to nothing.
func Record(db *storage.DB, task string, started time.Time, status, output, message, sessionID, invocationID string) error {
	if _, err := db.Exec(`INSERT INTO schedule_runs
		(task,started_at,finished_at,status,output,error,session_id,invocation_id)
		VALUES(?,?,?,?,?,?,?,?)`,
		task, started, time.Now(), status, output, message, sessionID, invocationID); err != nil {
		return err
	}
	// Keep one month of operational history. The newest records remain available
	// in the dashboard while unattended schedules cannot grow state.db forever.
	_, err := db.Exec(`DELETE FROM schedule_runs WHERE finished_at < ?`, time.Now().AddDate(0, 0, -30))
	return err
}

// List returns a page of runs, newest first, optionally for one task.
func List(db *storage.DB, task string, limit, offset int) ([]Run, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	where, args := "", []any{}
	if task != "" {
		where, args = " WHERE task=?", []any{task}
	}

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schedule_runs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := db.Query(`SELECT id,task,started_at,finished_at,status,output,error,session_id,invocation_id
		FROM schedule_runs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var x Run
		if err := rows.Scan(&x.ID, &x.Task, &x.StartedAt, &x.FinishedAt, &x.Status,
			&x.Output, &x.Error, &x.SessionID, &x.InvocationID); err != nil {
			return nil, 0, err
		}
		out = append(out, x)
	}
	return out, total, rows.Err()
}

// open connects to the shared state database and brings the table up to date.
//
// The schema and the migration run here, once per connection, rather than
// inline in every query — which is where they used to be, and why the table
// could never gain a column.
// EnsureSchema creates this store's table and brings an existing one up to
// date. Called once when the shared handle is opened.
//
// The opener this replaced did both on every single call, and resolved its own
// path with session.DefaultDBPath() — ignoring a configured location, so a
// deployment that moved its state database had its schedule history quietly
// written somewhere else. Taking the shared handle fixes that by construction.
func EnsureSchema(db *storage.DB) error {
	if err := storage.ApplySchema(db, schema, "schedule_runs"); err != nil {
		return err
	}
	return migrate(db)
}
