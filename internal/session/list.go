package session

import (
	"database/sql"
	"fmt"

	// Pure-Go SQLite driver (modernc.org/sqlite), registered as "sqlite" — the
	// same driver the memory index uses, so both can open the shared state.db.
	_ "github.com/glebarez/go-sqlite"
)

// SessionMeta is a lightweight session row for the list UI: id, event count and
// last-update epoch (seconds) only — no event bodies, so listing stays cheap as
// history grows.
type SessionMeta struct {
	ID         string
	Events     int
	LastUpdate int64
}

// openDB opens the shared state.db for short read queries, mirroring the
// memory index's settings (single conn + WAL + busy_timeout) so it coexists with
// the session store's writer. Used for both the read projections and the delete
// helpers. An empty path resolves to DefaultDBPath. Caller closes the returned DB.
func openDB(dbPath string) (*sql.DB, error) {
	if dbPath == "" {
		p, err := DefaultDBPath()
		if err != nil {
			return nil, err
		}
		dbPath = p
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open session db %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("set %q: %w", pragma, err)
		}
	}
	return db, nil
}

// ListPage returns one page of sessions for app/user, newest first (update_time
// desc, id desc), plus the total count for the same filter. Event counts come
// from a COUNT join — ADK's session.List does not preload events, so this query
// is also the only place the real per-session count is available. A limit <= 0
// uses a default; offset < 0 is clamped to 0.
func ListPage(dbPath, appName, userID string, limit, offset int) (rows []SessionMeta, total int, err error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	db, err := openDB(dbPath)
	if err != nil {
		return nil, 0, err
	}
	defer db.Close()

	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE app_name = ? AND user_id = ?`,
		appName, userID,
	).Scan(&total); err != nil {
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
func AllIDs(dbPath, appName, userID string) ([]string, error) {
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	res, err := db.Query(
		`SELECT id FROM sessions WHERE app_name = ? AND user_id = ? ORDER BY update_time DESC, id DESC`,
		appName, userID,
	)
	if err != nil {
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
