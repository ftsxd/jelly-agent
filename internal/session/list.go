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
// AutoMigrate when the session service opens (see NewSQLite). A hand-written
// DDL here would be a second definition of somebody else's schema, silently
// drifting the next time ADK changes its models. The read and delete helpers
// in this package already tolerate the tables not being there yet.
func EnsureSchema(*storage.DB) error { return nil }

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

	const q = `
SELECT s.id,
       CAST(strftime('%s', s.update_time) AS INTEGER) AS last_update,
       COUNT(e.id) AS events
FROM sessions s
LEFT JOIN events e
  ON e.app_name = s.app_name AND e.user_id = s.user_id AND e.session_id = s.id
WHERE s.app_name = ? AND s.user_id = ?
GROUP BY s.id, s.update_time
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
