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
	"strconv"
	"strings"
	"time"

	// The same driver, registered under the same "sqlite" name, that the
	// session store, the FTS5 index and the metrics recorder already use. Not
	// interchangeable with importing modernc.org/sqlite directly: both
	// register that name, and a second registration panics the process at
	// init — which is what happened when this file first imported the other
	// one.

	"github.com/jelly-agent/jelly-agent/internal/ops"

	"github.com/jelly-agent/jelly-agent/internal/storage"
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
	Tool         string
	Server       string
	At           time.Time
	// Payload is what the tool returned to the gateway, before shaping and
	// before the gateway's own ceiling was applied.
	Payload  []byte
	Upstream Upstream
}

// Label is the model-visible handle for a delivery: e1, e2, … numbered within
// a session.
//
// Short because it travels in prompts and in citations, where a thirty-
// character call id would cost tokens on every tool result and make a report
// unreadable. Unique within a session and stable across restarts because the
// number comes from the store rather than from a counter in memory — a
// reference the model can still resolve tomorrow is the whole point.
func Label(seq int) string { return "e" + strconv.Itoa(seq) }

// parseLabel is Label's inverse. A malformed label resolves to nothing rather
// than to row zero.
//
// Digits only, checked before Atoi rather than left to it: Atoi accepts a
// leading sign, so "e+1" would otherwise be a second name for e1. Two handles
// for one delivery is the mirror of one handle for two — a citation stops
// being an identity.
func parseLabel(label string) (int, bool) {
	rest, ok := strings.CutPrefix(label, "e")
	if !ok || rest == "" {
		return 0, false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// Chunk is a slice of a stored payload.
type Chunk struct {
	CallID string
	Tool   string
	// Lines is the payload's line count. Carried on every read so "how big is
	// this" is answered by the first cheap read rather than by a separate
	// tool — one less schema in a prompt that pays for every one of them.
	Lines int

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
	-- seq numbers deliveries within a session, assigned here rather than by
	-- the caller. An in-process counter would reset on restart and hand the
	-- next turn a label that already refers to something else, which is the
	-- one thing a durable reference cannot do.
	seq           INTEGER NOT NULL DEFAULT 0,
	tool          TEXT    NOT NULL,
	server        TEXT    NOT NULL DEFAULT '',
	at            TEXT    NOT NULL,
	bytes         INTEGER NOT NULL,
	sha256        TEXT    NOT NULL,
	upstream      TEXT    NOT NULL DEFAULT 'unknown',
	-- Non-empty once retention has dropped the payload. The row stays so the
	-- handle keeps resolving; see retention.go for why that matters more than
	-- the hundred bytes it costs.
	expired_at    TEXT    NOT NULL DEFAULT '',
	payload       BLOB    NOT NULL,
	PRIMARY KEY (app_name, user_id, session_id, invocation_id, call_id)
);
CREATE INDEX IF NOT EXISTS idx_tool_results_session ON tool_results(session_id);
CREATE INDEX IF NOT EXISTS idx_tool_results_at      ON tool_results(at);
`

// The unique index is created after migrate, not in schema: on a database
// that predates the seq column, CREATE TABLE IF NOT EXISTS does nothing and
// the index would reference a column that is not there yet.
const seqIndex = `CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_results_seq
	ON tool_results(app_name, user_id, session_id, seq)`

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
	db, err := storage.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("record: create: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("record: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate brings an existing table up to the current shape.
//
// CREATE TABLE IF NOT EXISTS does nothing to a table that already exists, so a
// column added in a later version has to be added explicitly — and everything
// depending on it, the unique index here, has to come after. This is the
// lesson internal/metrics learned the same way: the create statement looks
// like it covers migration, and the failure lands at runtime on a machine that
// had been working.
//
// An earlier version of this table had a `label` column that nothing ever
// wrote. It is left in place rather than dropped: a vestigial column costs
// nothing, while a DROP COLUMN on a table holding real payloads is a rewrite
// with no upside.
func migrate(db *sql.DB) error {
	if err := storage.EnsureColumns(db, "tool_results", []storage.Column{
		{Name: "seq", DDL: "ALTER TABLE tool_results ADD COLUMN seq INTEGER NOT NULL DEFAULT 0"},
		{Name: "expired_at", DDL: "ALTER TABLE tool_results ADD COLUMN expired_at TEXT NOT NULL DEFAULT ''"},
	}); err != nil {
		return err
	}

	// Rows written before seq existed all default to zero, which the unique
	// index would reject the moment one session has two of them. Number them
	// by arrival, which is the order they would have been given.
	if _, err := db.Exec(`
		UPDATE tool_results SET seq = (
			SELECT COUNT(*) FROM tool_results AS earlier
			WHERE earlier.app_name = tool_results.app_name
			  AND earlier.user_id  = tool_results.user_id
			  AND earlier.session_id = tool_results.session_id
			  AND (earlier.at < tool_results.at
			       OR (earlier.at = tool_results.at AND earlier.call_id <= tool_results.call_id))
		) WHERE seq = 0`); err != nil {
		return fmt.Errorf("backfill seq: %w", err)
	}

	if _, err := db.Exec(seqIndex); err != nil {
		return fmt.Errorf("create seq index: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Put commits one delivery and returns the handle the model may cite.
//
// The store assigns the handle, and that is deliberate: it means a label
// cannot exist without the payload behind it. The caller publishes a reference
// only if this returns one, so an unresolvable citation is not merely
// unlikely — it is unrepresentable.
//
// The error is the other half of that contract. It is also why the write is a
// single statement rather than a best-effort goroutine: the caller needs the
// answer before it decides what to tell the model.
//
// Re-storing the same call replaces the payload and keeps the handle. That
// happens when an approved tool is re-executed under the same call id: the
// second delivery is the real one, but anything already citing the first
// label must still resolve.
func (s *Store) Put(ctx context.Context, r Record) (string, error) {
	if s == nil || s.db == nil {
		return "", errors.New("record: store not open")
	}
	if r.SessionID == "" || r.CallID == "" {
		return "", fmt.Errorf("record: need a session and a call id, got %q/%q", r.SessionID, r.CallID)
	}
	if r.Upstream == "" {
		r.Upstream = UpstreamUnknown
	}
	sum := sha256.Sum256(r.Payload)

	// One statement, so allocating the number and writing the row cannot come
	// apart. COALESCE(MAX(seq),0)+1 is evaluated against the session's rows;
	// the unique index on (scope, seq) turns any race into a failed write
	// rather than two deliveries sharing a handle.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tool_results
			(app_name,user_id,session_id,invocation_id,call_id,seq,tool,server,at,bytes,sha256,upstream,payload)
		VALUES (?,?,?,?,?,
			(SELECT COALESCE(MAX(seq),0)+1 FROM tool_results
			 WHERE app_name=? AND user_id=? AND session_id=?),
			?,?,?,?,?,?,?)
		ON CONFLICT(app_name,user_id,session_id,invocation_id,call_id) DO UPDATE SET
			tool=excluded.tool, server=excluded.server,
			at=excluded.at, bytes=excluded.bytes, sha256=excluded.sha256,
			upstream=excluded.upstream, payload=excluded.payload`,
		r.AppName, r.UserID, r.SessionID, r.InvocationID, r.CallID,
		r.AppName, r.UserID, r.SessionID,
		r.Tool, r.Server, r.At.UTC().Format(time.RFC3339Nano),
		len(r.Payload), hex.EncodeToString(sum[:]), string(r.Upstream), r.Payload)
	if err != nil {
		return "", fmt.Errorf("record: put %s/%s: %w", r.SessionID, r.CallID, err)
	}

	var seq int
	if err := s.db.QueryRowContext(ctx, `
		SELECT seq FROM tool_results
		WHERE app_name=? AND user_id=? AND session_id=? AND invocation_id=? AND call_id=?`,
		r.AppName, r.UserID, r.SessionID, r.InvocationID, r.CallID,
	).Scan(&seq); err != nil {
		return "", fmt.Errorf("record: read back handle for %s/%s: %w", r.SessionID, r.CallID, err)
	}
	return Label(seq), nil
}

// ReadLabel returns a window of the delivery a model-visible handle names.
//
// This is the path that closes the loop: the model was shown e7, the payload
// behind e7 was shortened before it reached the prompt, and this is how the
// rest of it comes back. It works across turns and across restarts because
// the handle was assigned by the store, not by a counter that a restart
// resets.
//
// A malformed handle is a miss, not row zero.
func (s *Store) ReadLabel(ctx context.Context, sc Scope, label string, offset, limit int) (Chunk, error) {
	seq, ok := parseLabel(label)
	if !ok {
		return Chunk{}, ErrNotFound
	}
	return s.read(ctx, sc, `seq=?`, seq, offset, limit)
}

// Read returns a window of one delivery, addressed by the tool call it
// answered. Used by the console, which knows call ids from the timeline.
//
// The invocation is required, and that is not tidiness: the primary key is
// (app, user, session, invocation, call), so a call id alone can match more
// than one row in a session — two runs of the same conversation routinely
// number their calls from zero. Reading on the call id alone returned an
// arbitrary one of them, which showed one run's evidence under another run's
// call. Prefer ReadLabel, whose key is unique per session by construction.
func (s *Store) Read(ctx context.Context, sc Scope, invocationID, callID string, offset, limit int) (Chunk, error) {
	if invocationID == "" || callID == "" {
		return Chunk{}, ErrNotFound
	}
	return s.readBy(ctx, sc, `invocation_id=? AND call_id=?`, []any{invocationID, callID}, offset, limit)
}

// read is the shared body. The scope is part of the WHERE clause rather than
// checked afterwards: a query that cannot return another session's row is a
// stronger guarantee than one that returns it and then compares.
//
// Windowed rather than whole because storing everything is not a licence to
// hand everything back: the payload that was too large for a prompt is still
// too large for a prompt, and a caller that wants it must say how much it
// wants. A limit of zero or less takes the default window.
func (s *Store) read(ctx context.Context, sc Scope, cond string, key any, offset, limit int) (Chunk, error) {
	return s.readBy(ctx, sc, cond, []any{key}, offset, limit)
}

func (s *Store) readBy(ctx context.Context, sc Scope, cond string, keys []any, offset, limit int) (Chunk, error) {
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
		c       Chunk
		at      string
		expired string
		seq     int
		buf     []byte
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT call_id,tool,server,at,seq,upstream,bytes,sha256,expired_at,payload
		FROM tool_results
		WHERE app_name=? AND user_id=? AND session_id=? AND `+cond,
		append([]any{sc.AppName, sc.UserID, sc.SessionID}, keys...)...,
	).Scan(&c.CallID, &c.Tool, &c.Server, &at, &seq, &c.Upstream, &c.Total, &c.SHA256, &expired, &buf)
	if errors.Is(err, sql.ErrNoRows) {
		return Chunk{}, ErrNotFound
	}
	if err != nil {
		return Chunk{}, fmt.Errorf("record: read %s/%v: %w", sc.SessionID, keys, err)
	}
	if expired != "" {
		// Reported before anything is decoded: the payload is gone, and
		// returning an empty window would read as "the tool returned nothing".
		return Chunk{}, fmt.Errorf("%w: %s 于 %s 过期（原本 %d 字节）",
			ErrExpired, Label(seq), expired, c.Total)
	}
	c.Label = Label(seq)
	// Read the text, not the envelope. Almost every MCP tool returns its whole
	// output in one JSON string field, where the newlines are the characters
	// backslash and n — so without this a 60000-line log is one line, offsets
	// step through escape sequences, and search has nothing to match on.
	//
	// Total is recomputed from the view rather than taken from the bytes
	// column, because an offset the caller sends back has to mean the same
	// thing as the total it was compared against.
	buf = ops.TextView(buf)
	c.Total = len(buf)
	c.Lines = ops.CountLines(buf)
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
