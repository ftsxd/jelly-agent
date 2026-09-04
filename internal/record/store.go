// Package record durably keeps what a tool handed back, so a result that was
// shortened for the prompt can still be read in full afterwards.
//
// Three things about the name. It is not "the original": a tool may already
// have cut its own output before the gateway ever saw it — fetch_url truncates
// to max_chars inside the tool, and the sandbox caps stdout — so what is kept
// here is what the tool *delivered*, which is a weaker and honest claim. When
// the tool says it truncated, that is recorded; when it says nothing, the
// answer is "unknown", never "no". Promising a recoverable original we do not
// have would be worse than not storing anything, because someone would rely
// on it.
//
// It is also not metrics. internal/metrics records that a call happened and is
// allowed to fail doing so — the row is observability, the call is the
// product. This store carries a guarantee instead: a reference is published
// only after the payload is committed, because a citation that cannot be
// resolved is worse than an absent one.
//
// Finally, the identity here is deliberately not the evidence label. Those are
// e1, e2, … minted from a per-process counter, so they are unique within one
// run and collide across sessions and restarts — which is exactly what a
// durable reference must not do. A record is addressed by its scope plus the
// call id ADK assigns, both of which survive a restart.
package record

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// The same driver, registered under the same "sqlite" name, that the
	// session store, the FTS5 index and the metrics recorder already use. Not
	// interchangeable with importing modernc.org/sqlite directly: both
	// register that name, and a second registration panics the process at
	// init — which is what happened when this file first imported the other
	// one.
	_ "github.com/glebarez/go-sqlite"
)

// Upstream reports whether the tool had already shortened its own output.
//
// Unknown is the default and the common case: our own tools set a truncated
// flag, but a third-party MCP server has no such convention, and treating its
// silence as "complete" would be the same mistake the gateway's side-effect
// handling had to unlearn.
type Upstream string

const (
	UpstreamUnknown Upstream = "unknown"
	UpstreamNo      Upstream = "no"
	UpstreamYes     Upstream = "yes"
)

// Scope is the ownership boundary. Every read is checked against it, so a
// reference from one conversation cannot pull a payload out of another.
type Scope struct {
	AppName   string
	UserID    string
	SessionID string
}

// Record is one tool delivery.
type Record struct {
	Scope
	InvocationID string
	CallID       string
	// Label is the in-run evidence name (e1, e2, …). Stored so a report that
	// cites it can be resolved back to this row, and explicitly not used as
	// the key.
	Label  string
	Tool   string
	Server string
	At     time.Time
	// Payload is what the tool returned to the gateway, before shaping and
	// before the gateway's own ceiling was applied.
	Payload  []byte
	Upstream Upstream
}

// Chunk is a slice of a stored payload.
type Chunk struct {
	Tool     string
	Server   string
	At       time.Time
	Label    string
	Upstream Upstream
	// Total is the payload's full length in bytes; Offset and Data describe
	// the piece returned. Stored completely is not the same as handed back
	// completely — a caller reads a window, deliberately.
	Total  int
	Offset int
	Data   []byte
	// SHA256 covers the whole payload, so a caller reassembling several
	// windows can tell whether they belong together.
	SHA256 string
}

// ErrNotFound means no record matches, either because none was stored or
// because it belongs to another scope. The two are deliberately
// indistinguishable: telling a caller that a record exists but is not theirs
// leaks which sessions exist.
var ErrNotFound = errors.New("record: not found")

const schema = `
CREATE TABLE IF NOT EXISTS tool_results (
	app_name      TEXT    NOT NULL,
	user_id       TEXT    NOT NULL,
	session_id    TEXT    NOT NULL,
	invocation_id TEXT    NOT NULL,
	call_id       TEXT    NOT NULL,
	label         TEXT    NOT NULL DEFAULT '',
	tool          TEXT    NOT NULL,
	server        TEXT    NOT NULL DEFAULT '',
	at            TEXT    NOT NULL,
	bytes         INTEGER NOT NULL,
	sha256        TEXT    NOT NULL,
	upstream      TEXT    NOT NULL DEFAULT 'unknown',
	payload       BLOB    NOT NULL,
	PRIMARY KEY (app_name, user_id, session_id, invocation_id, call_id)
);
CREATE INDEX IF NOT EXISTS idx_tool_results_session ON tool_results(session_id);
CREATE INDEX IF NOT EXISTS idx_tool_results_at      ON tool_results(at);
`

// Store keeps tool deliveries in SQLite.
//
// One implementation on purpose. The interface the gateway depends on lives
// beside it so a networked database can be added when the deployment needs
// one — several machines sharing the data, or rolling restarts — but writing a
// second implementation now would be guessing at a schema nobody has had to
// operate yet. Everything here is portable SQL: no SQLite-only types, no
// reliance on rowid, and the key is ours rather than an autoincrement.
type Store struct {
	db *sql.DB
}

// Open creates or migrates the table at dbPath.
func Open(dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("record: empty db path")
	}
	if dir := filepath.Dir(dbPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("record: create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("record: open %s: %w", dbPath, err)
	}
	// Same settings as the other openers on this file: one writer, WAL so this
	// handle coexists with the session store's, and a busy timeout so a brief
	// write lock waits instead of failing. Unlike the metrics recorder, a
	// failure here is not something to shrug off — see Put.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA synchronous=NORMAL`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("record: %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("record: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Put commits one delivery.
//
// The error is the whole point of this method: the caller must not publish a
// reference to a payload that was not written. It is also why the write is a
// single statement rather than a best-effort goroutine — the caller needs the
// answer before it decides what to tell the model.
//
// Re-storing the same call replaces the row. That happens when an approved
// tool is re-executed under the same call id, and the second delivery is the
// real one.
func (s *Store) Put(ctx context.Context, r Record) error {
	if s == nil || s.db == nil {
		return errors.New("record: store not open")
	}
	if r.SessionID == "" || r.CallID == "" {
		return fmt.Errorf("record: need a session and a call id, got %q/%q", r.SessionID, r.CallID)
	}
	if r.Upstream == "" {
		r.Upstream = UpstreamUnknown
	}
	sum := sha256.Sum256(r.Payload)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tool_results
			(app_name,user_id,session_id,invocation_id,call_id,label,tool,server,at,bytes,sha256,upstream,payload)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(app_name,user_id,session_id,invocation_id,call_id) DO UPDATE SET
			label=excluded.label, tool=excluded.tool, server=excluded.server,
			at=excluded.at, bytes=excluded.bytes, sha256=excluded.sha256,
			upstream=excluded.upstream, payload=excluded.payload`,
		r.AppName, r.UserID, r.SessionID, r.InvocationID, r.CallID, r.Label,
		r.Tool, r.Server, r.At.UTC().Format(time.RFC3339Nano),
		len(r.Payload), hex.EncodeToString(sum[:]), string(r.Upstream), r.Payload)
	if err != nil {
		return fmt.Errorf("record: put %s/%s: %w", r.SessionID, r.CallID, err)
	}
	return nil
}

// Read returns a window of one delivery.
//
// Windowed rather than whole because storing everything is not a licence to
// hand everything back: the payload that was too large for a prompt is still
// too large for a prompt, and a caller that wants it must say how much it
// wants. A limit of zero or less takes the default window.
func (s *Store) Read(ctx context.Context, sc Scope, callID string, offset, limit int) (Chunk, error) {
	if s == nil || s.db == nil {
		return Chunk{}, errors.New("record: store not open")
	}
	if limit <= 0 {
		limit = DefaultWindow
	}
	if limit > MaxWindow {
		limit = MaxWindow
	}
	if offset < 0 {
		offset = 0
	}

	var (
		c   Chunk
		at  string
		buf []byte
	)
	// Scope is part of the WHERE clause, not checked afterwards: a query that
	// cannot return another session's row is a stronger guarantee than one
	// that returns it and then compares.
	err := s.db.QueryRowContext(ctx, `
		SELECT tool,server,at,label,upstream,bytes,sha256,payload
		FROM tool_results
		WHERE app_name=? AND user_id=? AND session_id=? AND call_id=?`,
		sc.AppName, sc.UserID, sc.SessionID, callID,
	).Scan(&c.Tool, &c.Server, &at, &c.Label, &c.Upstream, &c.Total, &c.SHA256, &buf)
	if errors.Is(err, sql.ErrNoRows) {
		return Chunk{}, ErrNotFound
	}
	if err != nil {
		return Chunk{}, fmt.Errorf("record: read %s/%s: %w", sc.SessionID, callID, err)
	}
	c.At, _ = time.Parse(time.RFC3339Nano, at)

	if offset >= len(buf) {
		c.Offset = len(buf)
		c.Data = nil
		return c, nil
	}
	end := min(offset+limit, len(buf))
	c.Offset = offset
	c.Data = buf[offset:end]
	return c, nil
}

// Window bounds on a single read. A caller that wants more pages for more.
const (
	DefaultWindow = 8 << 10   // 8 KiB
	MaxWindow     = 256 << 10 // 256 KiB
)

// Delete removes a session's records, for when the session itself is deleted.
func (s *Store) Delete(ctx context.Context, sc Scope) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM tool_results WHERE app_name=? AND user_id=? AND session_id=?`,
		sc.AppName, sc.UserID, sc.SessionID)
	return err
}
