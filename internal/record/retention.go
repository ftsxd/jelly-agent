package record

// How long a delivery stays readable.
//
// Everything a tool delivers is committed, so without a bound the store grows
// with every call the process ever serves. A long-running deployment would
// accumulate every log dump and every alert list forever, which is not a
// feature: a result nobody has cited in a month is not going to be cited.
//
// Expiry drops the payload but keeps the row.
//
// That is the whole design, and the reason is the same one that made the store
// assign its own handles: a reference must not become ambiguous. If the row
// were deleted, a handle from an old turn would come back as ErrNotFound —
// indistinguishable from a typo, from another session's handle, and from a
// call whose delivery never landed. The model would be told "no such result"
// when the truth is "that result is gone", and those call for different next
// moves: one means look again, the other means re-run the tool.
//
// A tombstone costs about a hundred bytes and keeps the answer precise. At a
// thousand tool calls a day that is under forty megabytes a year, which is
// cheaper than the ambiguity. If a deployment ever needs the rows themselves
// reclaimed, a second and much longer tier can delete tombstones — but adding
// that knob now would be guessing at an operational problem nobody has had.

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrExpired means the handle is real and belonged to this session, but the
// payload is past its retention. Distinct from ErrNotFound on purpose — see
// the note above.
var ErrExpired = errors.New("record: expired")

// DefaultRetention is how long a delivery stays readable when nothing is
// configured. A week covers the span over which a diagnosis is still being
// re-read or handed to someone else; past that the conversation itself is
// usually gone too.
const DefaultRetention = 7 * 24 * time.Hour

// Expire drops the payloads of deliveries older than cutoff, keeping their
// rows as tombstones. It reports how many it expired.
//
// Idempotent: a row already expired is skipped, so a sweeper that runs on a
// timer and again at startup does no extra work and reports honest counts.
func (s *Store) Expire(ctx context.Context, cutoff time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("record: store not open")
	}
	// The empty payload is a parameter, not a literal.
	//
	// It used to be X'', which is SQLite's blob literal and not assignable to
	// PostgreSQL's bytea. That failed the whole statement — so on PostgreSQL
	// the sweep at startup and every hour after it errored out, and stored
	// results never expired at all: the one job this file exists to do.
	res, err := s.db.ExecContext(ctx, `
		UPDATE tool_results
		SET payload = ?, expired_at = ?
		WHERE at < ? AND expired_at = ''`,
		[]byte{},
		time.Now().UTC().Format(time.RFC3339Nano),
		cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("record: expire before %s: %w", cutoff.Format(time.RFC3339), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// The update succeeded; only the count is unavailable. Reporting an
		// error here would make a caller think nothing was expired.
		return 0, nil
	}
	return n, nil
}

// Sweep expires everything older than retention, measured from now.
//
// A non-positive retention keeps everything and is a supported choice: an
// operator who wants the store to be an archive should not have to pick a
// number large enough to never fire.
func (s *Store) Sweep(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	return s.Expire(ctx, time.Now().Add(-retention))
}
