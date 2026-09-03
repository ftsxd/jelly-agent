package metrics

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	// Pure-Go SQLite driver, registered as "sqlite" — the same driver and the
	// same state.db the session store and the FTS5 index already use, so this
	// adds no new file and no new dependency.
	_ "github.com/glebarez/go-sqlite"
)

// maxArgsChars bounds the stored argument JSON. Arguments are kept for
// attribution ("which parameters was it getting wrong"), not for replay, so a
// long one can be cut without losing that.
const maxArgsChars = 2000

// ToolCall is one recorded invocation. Every field is filled at the call site;
// nothing here is derived later.
type ToolCall struct {
	At           time.Time
	SessionID    string
	InvocationID string
	Agent        string
	CallID       string // ADK FunctionCallID, unique per call
	Tool         string
	Args         map[string]any
	Duration     time.Duration
	OK           bool
	ErrKind      ErrKind
	Err          string
	ResultBytes  int
}

// Recorder appends tool-call rows to state.db.
//
// Writes are synchronous and unbatched: one local SQLite insert per tool call,
// against a call that just spent hundreds of milliseconds talking to the
// outside world. Buffering would trade a rounding error in latency for the
// possibility of losing exactly the rows a crash makes most interesting.
type Recorder struct {
	mu sync.Mutex
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS tool_calls (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	at            TEXT    NOT NULL,
	session_id    TEXT    NOT NULL DEFAULT '',
	invocation_id TEXT    NOT NULL DEFAULT '',
	agent         TEXT    NOT NULL DEFAULT '',
	call_id       TEXT    NOT NULL DEFAULT '',
	tool          TEXT    NOT NULL,
	args          TEXT    NOT NULL DEFAULT '',
	duration_ms   INTEGER NOT NULL DEFAULT 0,
	ok            INTEGER NOT NULL,
	err_kind      TEXT    NOT NULL DEFAULT '',
	err           TEXT    NOT NULL DEFAULT '',
	result_bytes  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_tool_calls_at      ON tool_calls(at);
CREATE INDEX IF NOT EXISTS idx_tool_calls_session ON tool_calls(session_id);
CREATE INDEX IF NOT EXISTS idx_tool_calls_tool    ON tool_calls(tool);
`

// NewRecorder opens (and migrates) the tool-call table at dbPath, creating
// parent directories as needed.
func NewRecorder(dbPath string) (*Recorder, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("metrics: empty db path")
	}
	if dir := filepath.Dir(dbPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("metrics: create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("metrics: open %s: %w", dbPath, err)
	}
	// One writer at a time; the session store, the FTS5 index and the schedule
	// store each hold their own handle on this same file, so keeping this pool
	// at one connection avoids piling up writers.
	db.SetMaxOpenConns(1)
	// Same settings the other openers in this repo use: WAL lets this writer
	// coexist with the session store's connection, and busy_timeout waits out a
	// brief write lock instead of failing the insert outright. Without these,
	// recording a tool call fails with "database is locked" whenever the
	// session store happens to be flushing — losing exactly the rows a busy
	// run produces.
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("metrics: %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("metrics: migrate: %w", err)
	}
	return &Recorder{db: db}, nil
}

// Record appends one row. A nil Recorder is a no-op, so callers that failed to
// open the database do not need to branch at every call site.
func (r *Recorder) Record(c ToolCall) error {
	if r == nil || r.db == nil {
		return nil
	}
	at := c.At
	if at.IsZero() {
		at = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(
		`INSERT INTO tool_calls
		 (at, session_id, invocation_id, agent, call_id, tool, args,
		  duration_ms, ok, err_kind, err, result_bytes)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		at.UTC().Format(time.RFC3339Nano), c.SessionID, c.InvocationID, c.Agent,
		c.CallID, c.Tool, encodeArgs(c.Args), c.Duration.Milliseconds(),
		boolToInt(c.OK), string(c.ErrKind), truncate(c.Err, 1000), c.ResultBytes,
	)
	if err != nil {
		return fmt.Errorf("metrics: insert: %w", err)
	}
	return nil
}

// Close releases the database handle. A nil Recorder closes cleanly.
func (r *Recorder) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.db.Close()
	r.db = nil
	return err
}

func encodeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return truncate(string(b), maxArgsChars)
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	// Cut on a rune boundary so the stored text stays valid UTF-8.
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
