package session

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// EnsureSchema is a no-op, and says so rather than being absent.
//
// The sessions and events tables belong to ADK, which creates them with GORM
// AutoMigrate when the session service opens (see New). A hand-written
// DDL here would be a second definition of somebody else's schema, silently
// drifting the next time ADK changes its models. The read and delete helpers
// in this package already tolerate the tables not being there yet.
// EnsureSchema is a no-op: the tables belong to ADK. The indexes on them do
// not — see EnsureIndexes, which runs after AutoMigrate has created the
// tables to put them on.
func EnsureSchema(*storage.DB) error { return nil }

// EnsureIndexes adds the indexes ADK's tables need and ADK does not create.
//
// Its models declare only composite primary keys — events is keyed
// (id, app_name, user_id, session_id), with id first — so "the events of this
// session" and "sessions newest first" both scan. Measured with 650 sessions:
// the task scan's fifteen pages took 3.8 seconds on SQLite and 3.1 on
// PostgreSQL, essentially all of it here.
//
// Added rather than declared: writing out ADK's tables would be a second
// definition of someone else's schema, drifting the next time they change a
// model. An index is additive — it names columns that have to exist for the
// table to work at all.
//
// Called after AutoMigrate, because that is when there is a table to put them
// on.
func EnsureIndexes(db *storage.DB) error {
	for _, ddl := range []string{
		`CREATE INDEX IF NOT EXISTS idx_events_session
			ON events (app_name, user_id, session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_recent
			ON sessions (app_name, user_id, update_time DESC, id DESC)`,
	} {
		if _, err := db.Exec(ddl); err != nil && !storage.IsMissingTable(err) {
			return fmt.Errorf("session: create index: %w", err)
		}
	}
	return nil
}

// SessionMeta is a lightweight session row for the list UI: id, event count and
// last-update epoch (seconds) only — no event bodies, so listing stays cheap as
// history grows.
type SessionMeta struct {
	ID         string
	Events     int
	LastUpdate int64
}

// ListPage returns one page of sessions for app/user, newest first (update_time
// desc, id desc), plus the total count for the same filter. Event counts come
// from a COUNT join — ADK's session.List does not preload events, so this query
// is also the only place the real per-session count is available. A limit <= 0
// uses a default; offset < 0 is clamped to 0.
func ListPage(db *storage.DB, appName, userID string, limit, offset int) (rows []SessionMeta, total int, err error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE app_name = ? AND user_id = ?`,
		appName, userID,
	).Scan(&total); err != nil {
		if isMissingTable(err) {
			return nil, 0, nil // the schema arrives with the first session
		}
		return nil, 0, fmt.Errorf("count sessions: %w", err)
	}

	// The event count is a correlated subquery, not a join with a GROUP BY.
	//
	// Joining and grouping computes a count for every session in the table
	// and only then applies LIMIT, so one page costs a pass over every event
	// — and the task scan walks up to fifteen pages. Measured on SQLite with
	// 650 sessions: 3.8 seconds for the walk, against 30ms for reading all
	// their events. A subquery in the projection is evaluated for the rows
	// that survive LIMIT, which is the page.
	q := `
SELECT s.id,
       ` + db.EpochSeconds("s.update_time") + ` AS last_update,
       (SELECT COUNT(*) FROM events e
         WHERE e.app_name = s.app_name AND e.user_id = s.user_id
           AND e.session_id = s.id) AS events
FROM sessions s
WHERE s.app_name = ? AND s.user_id = ?
ORDER BY s.update_time DESC, s.id DESC
LIMIT ? OFFSET ?`
	res, err := db.Query(q, appName, userID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list sessions: %w", err)
	}
	defer res.Close()
	for res.Next() {
		var m SessionMeta
		var lu sql.NullInt64
		if err := res.Scan(&m.ID, &lu, &m.Events); err != nil {
			return nil, 0, err
		}
		m.LastUpdate = lu.Int64
		rows = append(rows, m)
	}
	return rows, total, res.Err()
}

// AllIDs returns every session id for app/user (id only — cheap), newest first.
// Backs the "select all across pages" action so batch delete can target the
// full filtered set without paging through it.
func AllIDs(db *storage.DB, appName, userID string) ([]string, error) {
	res, err := db.Query(
		`SELECT id FROM sessions WHERE app_name = ? AND user_id = ? ORDER BY update_time DESC, id DESC`,
		appName, userID,
	)
	if err != nil {
		if isMissingTable(err) {
			return nil, nil // the schema is created on first use; nothing here yet
		}
		return nil, fmt.Errorf("list session ids: %w", err)
	}
	defer res.Close()
	var ids []string
	for res.Next() {
		var id string
		if err := res.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, res.Err()
}

// Exists reports whether a session is still in the store.
//
// The delivery endpoints need it: they are scoped by session id in SQL, which
// stops one conversation reaching another's payload, but says nothing about a
// conversation that was deleted. Rows are removed with the session now, so
// this is the second lock on the same door — and the one that still holds if a
// delete only half succeeded.
func Exists(db *storage.DB, appName, userID, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	var one int
	err := db.QueryRow(
		`SELECT 1 FROM sessions WHERE app_name = ? AND user_id = ? AND id = ?`,
		appName, userID, id,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		if isMissingTable(err) {
			return false, nil // the schema arrives with the first session
		}
		return false, fmt.Errorf("session exists %s: %w", id, err)
	}
	return true, nil
}
