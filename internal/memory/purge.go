package memory

import (
	"context"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// openIndexDB opens the shared state.db for the L2 index maintenance helpers
// below. Caller closes.
func openIndexDB(dbPath string) (*storage.DB, error) {
	return storage.Open(dbPath)
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

	// Chunked to stay under the placeholder limit, and all of it in one
	// transaction: this runs alongside the session and task purges, and a
	// half-purged index leaves searchable text for a session that is gone.
	n := 0
	err = db.InTx(context.Background(), func(tx *storage.Tx) error {
		return storage.ForEachChunk(ids, func(chunk []string) error {
			args := make([]any, len(chunk))
			for i, id := range chunk {
				args[i] = id
			}
			res, err := tx.Exec(
				`DELETE FROM memory_fts WHERE session_id IN (`+storage.Placeholders(len(chunk))+`)`,
				args...)
			if err != nil {
				return err
			}
			c, _ := res.RowsAffected()
			n += int(c)
			return nil
		})
	})
	if err != nil {
		return 0, tolerateMissing(err)
	}
	return n, nil
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
