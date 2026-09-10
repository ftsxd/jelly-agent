package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	adksession "google.golang.org/adk/session"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// EventsOf reads the events of several sessions in one query.
//
// ADK's session service has no batch API: getting a session's events means
// Get, one session at a time. The task list projects a page of sessions and
// so made one round trip per session — free against a local file, 20ms each
// against PostgreSQL, and the scan ceiling is 600 sessions. That is a cost
// that grows with the number of sessions rather than with the size of the
// page, which is the definition of the problem.
//
// Reading the table directly is safe here in a way it usually is not, because
// ADK stores these columns as ordinary JSON of its own public types
// (see database.createEventFromStorageEvent, which does the same decode).
// The one field of session.Event that this cannot produce is FinishReason,
// and ADK does not persist it either — Get returns it empty too.
//
// The tables belong to ADK and are created by its AutoMigrate; this only ever
// reads them.
func EventsOf(ctx context.Context, db *storage.DB, appName, userID string, sessionIDs []string) (map[string][]*adksession.Event, error) {
	out := make(map[string][]*adksession.Event, len(sessionIDs))
	kept := make([]string, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		if id != "" {
			kept = append(kept, id)
			out[id] = nil // an empty session is a session with no events, not a missing one
		}
	}
	if len(kept) == 0 {
		return out, nil
	}

	err := storage.ForEachChunk(kept, func(chunk []string) error {
		args := make([]any, 0, len(chunk)+2)
		args = append(args, appName, userID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, `
			SELECT session_id, id, invocation_id, author, branch, timestamp,
			       content, actions, usage_metadata, partial,
			       error_code, error_message
			FROM events
			WHERE app_name = ? AND user_id = ? AND session_id IN (`+
			storage.Placeholders(len(chunk))+`)
			ORDER BY session_id, timestamp, id`, args...)
		if err != nil {
			if storage.IsMissingTable(err) {
				// ADK creates these on first use. Before that there are no
				// events, which is a state and not a failure.
				return nil
			}
			return fmt.Errorf("session: read events: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			session, ev, err := scanEvent(rows)
			if err != nil {
				return err
			}
			out[session] = append(out[session], ev)
		}
		return rows.Err()
	})
	return out, err
}

// scanEvent decodes one row into the event the projection expects.
//
// Only the fields internal/server/timeline.go reads. Decoding the rest would
// be work whose result nothing looks at, and every column decoded is a column
// this has to keep agreeing with ADK about.
func scanEvent(rows rowScanner) (string, *adksession.Event, error) {
	var (
		sessionID, id, invocation, author string
		branch, errCode, errMsg           *string
		ts                                time.Time
		content, actions, usage           []byte
		partial                           *bool
	)
	if err := rows.Scan(&sessionID, &id, &invocation, &author, &branch, &ts,
		&content, &actions, &usage, &partial, &errCode, &errMsg); err != nil {
		return "", nil, fmt.Errorf("session: scan event: %w", err)
	}

	ev := &adksession.Event{
		ID: id, InvocationID: invocation, Author: author, Timestamp: ts,
	}
	if branch != nil {
		ev.Branch = *branch
	}
	if partial != nil {
		ev.Partial = *partial
	}
	if errCode != nil {
		ev.ErrorCode = *errCode
	}
	if errMsg != nil {
		ev.ErrorMessage = *errMsg
	}
	for _, f := range []struct {
		raw []byte
		dst any
		as  string
	}{
		{content, &ev.Content, "content"},
		{actions, &ev.Actions, "actions"},
		{usage, &ev.UsageMetadata, "usage metadata"},
	} {
		if len(f.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(f.raw, f.dst); err != nil {
			// One unreadable column must not lose the event: the projection
			// still has its author, its round and its timestamp, and a
			// timeline missing one message is better than a page that fails.
			continue
		}
	}
	return sessionID, ev, nil
}

// rowScanner is what both *sql.Rows and *sql.Row satisfy, so scanEvent can be
// tested without a query.
type rowScanner interface {
	Scan(dest ...any) error
}
