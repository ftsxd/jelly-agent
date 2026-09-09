package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/storage"
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

	deleted := 0
	err = db.InTx(context.Background(), func(tx *storage.Tx) error {
		for _, id := range ids {
			if id == "" {
				continue
			}
			if _, err := tx.Exec(
				`DELETE FROM events WHERE app_name = ? AND user_id = ? AND session_id = ?`,
				appName, userID, id,
			); err != nil {
				if isMissingTable(err) {
					return errNoSchema
				}
				return fmt.Errorf("delete events for %q: %w", id, err)
			}
			res, err := tx.Exec(
				`DELETE FROM sessions WHERE app_name = ? AND user_id = ? AND id = ?`,
				appName, userID, id,
			)
			if err != nil {
				if isMissingTable(err) {
					return errNoSchema
				}
				return fmt.Errorf("delete session %q: %w", id, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				deleted += int(n)
			}
		}
		return nil
	})
	if errors.Is(err, errNoSchema) {
		// A store whose schema has not been created yet holds no sessions, so
		// there is nothing to delete and nothing went wrong. This became
		// reachable once the handlers started honouring the configured path
		// instead of silently deleting from the shared default: a fresh
		// deployment's first delete used to answer 500 with a SQL error in it.
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// errNoSchema unwinds the transaction without reporting a failure.
//
// Returning nil would commit; returning the driver's error would surface a
// missing table as a 500. A sentinel is the only way to say "roll back, and
// this is fine" through a callback that signals with error.
var errNoSchema = errors.New("session: schema not created yet")

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
		if isMissingTable(err) {
			return 0, nil // fresh DB, nothing to purge
		}
		return 0, fmt.Errorf("purge orphan events: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// isMissingTable reports the one SQL failure that means "this store is empty"
// rather than "this store is broken".
//
// The session schema is created by the ADK service on first use, so every path
// that reaches the file directly can arrive before it exists. Matching on the
// message is what the driver leaves available; the alternative — creating the
// schema from here — would put a second definition of ADK's tables in this
// repository.
func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}
