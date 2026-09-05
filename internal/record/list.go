package record

// Listing what a run delivered, without reading any of it.
//
// The store could only ever be addressed one row at a time — by call id, or by
// the handle the model was shown. That is the right shape for the model, which
// asks for a specific thing it was told about. It is the wrong shape for a
// console, which has to show what a run produced before anyone has asked for
// any of it.
//
// So this returns metadata only. The payloads are the large part by orders of
// magnitude — a single row can be megabytes — and a list view needs none of
// them. Loading them to build a list would be the same mistake the delivery
// budget exists to prevent, made on the other side of the wire.

import (
	"context"
	"fmt"
	"time"
)

// Item describes one stored delivery. No payload, by design.
type Item struct {
	Label        string    `json:"label"` // the handle the model was shown, e.g. "e7"
	CallID       string    `json:"call_id"`
	InvocationID string    `json:"invocation_id"`
	Tool         string    `json:"tool"`
	Server       string    `json:"server,omitempty"`
	At           time.Time `json:"at"`
	Bytes        int       `json:"bytes"`
	SHA256       string    `json:"sha256"`

	// Upstream is what the tool said about shortening its own output before
	// this store saw it: "yes", "no" or "unknown". Unknown is the honest
	// answer for a third-party server that reports nothing, and must not be
	// read as "this is everything".
	Upstream Upstream `json:"upstream"`

	// Expired says the payload is past its retention and has been dropped.
	// The row survives so the handle still resolves to a precise answer —
	// "that result is gone" rather than "no such result".
	Expired   bool      `json:"expired"`
	ExpiredAt time.Time `json:"expired_at,omitzero"`
}

// ListOpts bounds a listing.
type ListOpts struct {
	// InvocationID narrows to one run. Empty lists the whole session.
	InvocationID string
	Limit        int
	Offset       int
}

// Default and maximum page sizes for a listing.
const (
	DefaultListLimit = 100
	MaxListLimit     = 1000
)

// List returns the deliveries of a session, newest handle first.
func (s *Store) List(ctx context.Context, sc Scope, opts ListOpts) ([]Item, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("record: store not open")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	offset := max(0, opts.Offset)

	where := `WHERE app_name=? AND user_id=? AND session_id=?`
	args := []any{sc.AppName, sc.UserID, sc.SessionID}
	if opts.InvocationID != "" {
		where += ` AND invocation_id=?`
		args = append(args, opts.InvocationID)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, call_id, invocation_id, tool, server, at, bytes, sha256, upstream, expired_at
		FROM tool_results `+where+`
		ORDER BY seq ASC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, fmt.Errorf("record: list %s: %w", sc.SessionID, err)
	}
	defer rows.Close()

	var out []Item
	for rows.Next() {
		var (
			it      Item
			seq     int
			at      string
			expired string
		)
		if err := rows.Scan(&seq, &it.CallID, &it.InvocationID, &it.Tool, &it.Server,
			&at, &it.Bytes, &it.SHA256, &it.Upstream, &expired); err != nil {
			return nil, fmt.Errorf("record: scan listing: %w", err)
		}
		it.Label = Label(seq)
		it.At, _ = time.Parse(time.RFC3339Nano, at)
		if expired != "" {
			it.Expired = true
			it.ExpiredAt, _ = time.Parse(time.RFC3339Nano, expired)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
