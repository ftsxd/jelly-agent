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
	result_bytes  INTEGER NOT NULL DEFAULT 0,
	evidence_id   TEXT    NOT NULL DEFAULT '',
	replayed      INTEGER NOT NULL DEFAULT 0,
	retrievable   INTEGER NOT NULL DEFAULT 0
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
	if err := addMissingColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Recorder{db: db}, nil
}

// addMissingColumns brings an existing table up to the current schema.
//
// CREATE TABLE IF NOT EXISTS does nothing to a table that already exists, so a
// column added to the schema above is missing from every database created
// before it — and the insert fails at runtime, on a machine that has been
// running fine. Each column is added separately and a duplicate-column error
// is the expected outcome on an up-to-date database.
func addMissingColumns(db *sql.DB) error {
	for _, col := range []struct{ name, ddl string }{
		{"evidence_id", "ALTER TABLE tool_calls ADD COLUMN evidence_id TEXT NOT NULL DEFAULT ''"},
		{"replayed", "ALTER TABLE tool_calls ADD COLUMN replayed INTEGER NOT NULL DEFAULT 0"},
		{"retrievable", "ALTER TABLE tool_calls ADD COLUMN retrievable INTEGER NOT NULL DEFAULT 0"},
	} {
		var count int
		err := db.QueryRow(
			`SELECT count(*) FROM pragma_table_info('tool_calls') WHERE name = ?`, col.name,
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("metrics: inspect column %s: %w", col.name, err)
		}
		if count > 0 {
			continue
		}
		if _, err := db.Exec(col.ddl); err != nil {
			return fmt.Errorf("metrics: add column %s: %w", col.name, err)
		}
	}
	return nil
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

// RecordGatewayCall stores what the gateway actually did.
//
// This is the authoritative row, and it replaces recording from ADK's tool
// callback rather than joining it. The callback sees the model's arguments and
// whichever alias it used; the gateway sees the arguments after injection, the
// canonical tool name, its own cause bucket, and the evidence ID a conclusion
// will cite. Two rows per call would double every count, and the wrong one
// would win a tie.
func (r *Recorder) RecordGatewayCall(c GatewayCall) error {
	if r == nil || r.db == nil {
		return nil
	}
	at := c.StartedAt
	if at.IsZero() {
		at = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(
		`INSERT INTO tool_calls
		 (at, session_id, invocation_id, agent, call_id, tool, args,
		  duration_ms, ok, err_kind, err, result_bytes, evidence_id, replayed,
		  retrievable)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		at.UTC().Format(time.RFC3339Nano), c.SessionID, c.InvocationID, c.Agent,
		c.CallID, c.Tool, encodeArgs(c.Args), c.Duration.Milliseconds(),
		boolToInt(c.OK), c.ErrKind, truncate(c.Err, 1000), c.ResultBytes,
		c.EvidenceID, boolToInt(c.Replayed), boolToInt(c.Retrievable),
	)
	if err != nil {
		return fmt.Errorf("metrics: insert gateway call: %w", err)
	}
	return nil
}

// GatewayCall is one call as the gateway saw it.
type GatewayCall struct {
	SessionID    string
	InvocationID string
	Agent        string
	CallID       string

	Tool      string
	Args      map[string]any
	StartedAt time.Time
	Duration  time.Duration

	OK          bool
	ErrKind     string
	Err         string
	ResultBytes int
	Replayed    bool
	// Retrievable says the delivery behind this call reached durable storage.
	// Recorded so "how much of this run can still be read in full" is a query
	// rather than a guess — the guarantee is only worth having if it is
	// auditable after the fact.
	Retrievable bool

	// EvidenceID links the row to the observation a conclusion can cite. A
	// row without one either failed or produced nothing to cite — and that
	// distinction is what makes Seal's checks verifiable against the record.
	EvidenceID string
}
