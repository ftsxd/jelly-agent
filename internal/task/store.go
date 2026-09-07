// Package task records which runs belong to which piece of work.
//
// A task is a goal the user set, not a message and not a conversation. Most of
// the time one run answers it, and then the task's identity can be derived
// from that run — no row needed. But a task the user comes back to, to supply
// a threshold or authorise a change or just ask for more, is answered by
// several runs, and nothing in the event stream says they belong together.
//
// So this table exists for exactly that: the runs a caller explicitly attached
// to an earlier task. Everything else stays derived, which is why an empty
// table means "every run is its own task" rather than "no tasks" — including
// for all the data written before this existed.
package task

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/session"
)

const schema = `
CREATE TABLE IF NOT EXISTS task_runs (
	task_id       TEXT NOT NULL,
	session_id    TEXT NOT NULL,
	invocation_id TEXT NOT NULL,
	at            TEXT NOT NULL,
	PRIMARY KEY (session_id, invocation_id)
);
CREATE INDEX IF NOT EXISTS idx_task_runs_task ON task_runs(task_id);
`

// ID is the identity of a task.
//
// Derived from the run that opened it rather than random, so it stays readable
// and stays joinable: every other table is keyed by (session, invocation), and
// a task id that is those two spelled out can be traced by eye through any of
// them. It also means a run that was never linked to anything still has a
// well-formed id, which is what keeps historical data working.
func ID(sessionID, invocationID string) string { return sessionID + "/" + invocationID }

// Split takes an id apart. The session may itself contain no slash, and the
// invocation never does, so the first separator is the boundary.
func Split(id string) (sessionID, invocationID string) {
	i := strings.Index(id, "/")
	if i < 0 {
		return id, ""
	}
	return id[:i], id[i+1:]
}

// Link records that a run belongs to a task.
//
// Idempotent: re-running the same invocation (a retry, a reconnect) must not
// create a second membership, and re-linking it to a different task would be a
// caller error rather than a state change, so the row stands.
func Link(dbPath, taskID, sessionID, invocationID string) error {
	if taskID == "" || sessionID == "" || invocationID == "" {
		return fmt.Errorf("task: link needs a task, a session and an invocation")
	}
	if err := Owns(taskID, sessionID); err != nil {
		return err
	}
	db, err := open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO task_runs(task_id, session_id, invocation_id, at)
		VALUES(?,?,?,?) ON CONFLICT(session_id, invocation_id) DO NOTHING`,
		taskID, sessionID, invocationID, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// Owns rejects a task id that belongs to a different conversation.
//
// A task id begins with the session that opened it, so this is a property of
// the id and not a lookup. It is checked because the id arrives from the
// client: a run could otherwise claim membership of any task in any session,
// and the board would then show that other session's task wearing this run's
// steps and title. Runs are folded per session, so nothing downstream could
// have made sense of it either.
func Owns(taskID, sessionID string) error {
	if owner, _ := Split(taskID); owner != sessionID {
		return fmt.Errorf("task: 任务 %q 属于会话 %q，不能把 %q 的运行挂上去", taskID, owner, sessionID)
	}
	return nil
}

// OfSession maps a session's runs to the tasks they were attached to.
//
// Runs with no row are absent from the map, which the caller reads as "its own
// task" — the common case, and the only case for anything written before this
// table existed.
func OfSession(dbPath, sessionID string) (map[string]string, error) {
	db, err := open(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(
		`SELECT invocation_id, task_id FROM task_runs WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("task: runs of %s: %w", sessionID, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var inv, id string
		if err := rows.Scan(&inv, &id); err != nil {
			return nil, err
		}
		out[inv] = id
	}
	return out, rows.Err()
}

// open connects to the state database.
//
// dbPath is threaded through rather than resolved here, because the override
// exists precisely so a test or a second deployment can point somewhere else —
// and a store that resolves its own path ignores it. Two handlers already had
// this bug; a test writing links into the developer's real database is how
// this one surfaced. Empty still means the shared default.
func open(dbPath string) (*sql.DB, error) {
	p := dbPath
	if p == "" {
		var err error
		if p, err = session.DefaultDBPath(); err != nil {
			return nil, err
		}
	}
	if dir := filepath.Dir(p); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("task: create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		return nil, fmt.Errorf("task: open db: %w", err)
	}
	// The same settings every other opener on this file uses, and for the same
	// reason: this database is shared with the session store, the delivery
	// store and the metrics recorder, all of which are writing while a turn
	// runs. Without WAL a reader blocks the writer; without a busy timeout a
	// brief write lock fails the call outright, and the call here is the one
	// that records which task a run belongs to — losing it silently splits a
	// continuation off into a task of its own. One connection, because a
	// second one would queue behind the first for no gain on a file this
	// small.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA synchronous=NORMAL`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("task: %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("task: create table: %w", err)
	}
	return db, nil
}

// DeleteSessions drops the task memberships of the given sessions.
//
// Called when the sessions are deleted. A membership row names a session, an
// invocation and the task they belonged to, so it outlives the conversation it
// describes unless it goes with it.
func DeleteSessions(dbPath string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	db, err := open(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	n := 0
	for _, id := range ids {
		if id == "" {
			continue
		}
		res, err := db.Exec(`DELETE FROM task_runs WHERE session_id = ?`, id)
		if err != nil {
			return n, fmt.Errorf("task: delete runs of %s: %w", id, err)
		}
		if c, _ := res.RowsAffected(); c > 0 {
			n += int(c)
		}
	}
	return n, nil
}
