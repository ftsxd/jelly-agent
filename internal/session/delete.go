package session

import (
	"fmt"
	"strings"
)

// DeleteSessions removes the given sessions and their events in one transaction.
// SQLite enforces foreign keys only when PRAGMA foreign_keys=ON (off by default),
// so ADK's OnDelete:CASCADE never fires and svc.Delete leaves orphaned event
// rows behind — this deletes both explicitly. Missing ids are a no-op
// (idempotent); it returns how many session rows were actually removed. Deleting
// events directly also handles ids containing "/" that the REST path-param route
// cannot match.
func DeleteSessions(dbPath, appName, userID string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	db, err := openDB(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, err := tx.Exec(
			`DELETE FROM events WHERE app_name = ? AND user_id = ? AND session_id = ?`,
			appName, userID, id,
		); err != nil {
			tx.Rollback()
			return deleted, fmt.Errorf("delete events for %q: %w", id, err)
		}
		res, err := tx.Exec(
			`DELETE FROM sessions WHERE app_name = ? AND user_id = ? AND id = ?`,
			appName, userID, id,
		)
		if err != nil {
			tx.Rollback()
			return deleted, fmt.Errorf("delete session %q: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			deleted += int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// PurgeOrphanEvents deletes event rows with no parent session — leftovers from
// past deletes that ran before sessions and events were removed together. It is
// best-effort: a brand-new database without the events table yet returns 0 and
// no error. Returns the number of rows removed.
func PurgeOrphanEvents(dbPath string) (int, error) {
	db, err := openDB(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	res, err := db.Exec(`DELETE FROM events WHERE NOT EXISTS (
		SELECT 1 FROM sessions s
		WHERE s.app_name = events.app_name
		  AND s.user_id = events.user_id
		  AND s.id = events.session_id)`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil // fresh DB, nothing to purge
		}
		return 0, fmt.Errorf("purge orphan events: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
