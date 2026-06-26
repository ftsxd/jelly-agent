package memory

import (
	"database/sql"
	"fmt"
	"strings"
)

// openIndexDB opens the shared state.db for the L2 index maintenance helpers
// below, mirroring NewSearch's connection settings (single conn + WAL +
// busy_timeout). Caller closes.
func openIndexDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open memory index %s: %w", dbPath, err)
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

// tolerateMissing returns nil when err is a "no such table" error — the L2 index
// (memory_fts) only exists once search has been enabled, so purging when it was
// never created is a no-op rather than a failure.
func tolerateMissing(err error) error {
	if err == nil || strings.Contains(err.Error(), "no such table") {
		return nil
	}
	return err
}

// PurgeSessions removes the L2 search-index rows for the given session ids. The
// index lives in a separate FTS5 table (memory_fts) that the session-store
// delete does not touch, so without this a deleted session's text stays
// retrievable via load_memory. Best-effort and idempotent; safe to call even
// when L2 search was never enabled. Returns the number of index rows removed.
func PurgeSessions(dbPath string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	db, err := openIndexDB(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	res, err := db.Exec(
		`DELETE FROM memory_fts WHERE session_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, tolerateMissing(err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// PurgeOrphanIndex removes L2 index rows whose session no longer exists —
// leftovers from sessions deleted before the index was purged alongside them.
// Best-effort: a missing memory_fts or sessions table yields 0 and no error.
func PurgeOrphanIndex(dbPath string) (int, error) {
	db, err := openIndexDB(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	res, err := db.Exec(`DELETE FROM memory_fts WHERE NOT EXISTS (
		SELECT 1 FROM sessions s
		WHERE s.app_name = memory_fts.app_name
		  AND s.user_id = memory_fts.user_id
		  AND s.id = memory_fts.session_id)`)
	if err != nil {
		return 0, tolerateMissing(err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
