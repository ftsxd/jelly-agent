package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	// Pure-Go SQLite driver (modernc.org/sqlite), registered as "sqlite".
	// FTS5 (incl. the trigram tokenizer) is compiled in, so no CGO is needed —
	// this keeps the single-binary deployment goal.
	_ "github.com/glebarez/go-sqlite"

	adkmemory "google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

const (
	// defaultTopK bounds how many entries SearchMemory returns; history is
	// never replayed wholesale (PLAN §10.4).
	defaultTopK = 5

	// ftsTable is the FTS5 virtual table indexing past-session text. It lives in
	// the same state.db as the session store (PLAN §10.3). The trigram tokenizer
	// gives substring matching that works for both CJK and ASCII, unlike the
	// default unicode61 tokenizer (which treats a whole Chinese sentence as one
	// token). Trade-off: trigram needs queries of ≥3 runes — shorter queries
	// fall back to a LIKE scan (see SearchMemory).
	createFTS = `CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
		content,
		event_id UNINDEXED,
		session_id UNINDEXED,
		author UNINDEXED,
		app_name UNINDEXED,
		user_id UNINDEXED,
		ts UNINDEXED,
		tokenize = 'trigram'
	)`
)

// Search is an L2 session-search memory.Service backed by SQLite FTS5. It
// indexes the text of past-session events and returns the top-K matches for a
// query, scoped to one (app, user). It satisfies google.golang.org/adk/memory's
// Service interface so it can be wired into runner.Config.MemoryService.
type Search struct {
	db   *sql.DB
	topK int
}

var _ adkmemory.Service = (*Search)(nil)

// NewSearch opens the FTS5 index on the shared state.db at dbPath, creating the
// virtual table if needed. A non-positive topK falls back to the default. The
// connection pool is capped at one connection so the per-connection PRAGMAs
// (WAL + busy_timeout) hold for every query and coexist with the session
// store's separate GORM connection to the same file. Call Close when done.
func NewSearch(dbPath string, topK int) (*Search, error) {
	if topK <= 0 {
		topK = defaultTopK
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open memory index %s: %w", dbPath, err)
	}
	// One connection keeps the PRAGMAs below effective for all queries.
	db.SetMaxOpenConns(1)
	// WAL lets our reader/writer coexist with the session store's connection;
	// busy_timeout waits out brief write locks instead of erroring immediately.
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("set %q: %w", pragma, err)
		}
	}
	if _, err := db.Exec(createFTS); err != nil {
		db.Close()
		return nil, fmt.Errorf("create memory_fts: %w", err)
	}
	return &Search{db: db, topK: topK}, nil
}

// Close releases the underlying database connection.
func (s *Search) Close() error { return s.db.Close() }

// AddSessionToMemory indexes the text content of a session's events. It is
// idempotent: re-adding a session replaces its prior rows, so calling it after
// every turn keeps the index current without duplicating entries. Events with
// no plain text (tool calls, pure-reasoning turns) are skipped.
func (s *Search) AddSessionToMemory(ctx context.Context, sess session.Session) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin memory tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE session_id = ?`, sess.ID()); err != nil {
		return fmt.Errorf("clear prior session rows: %w", err)
	}

	const insert = `INSERT INTO memory_fts(content, event_id, session_id, author, app_name, user_id, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	for ev := range sess.Events().All() {
		if ev.Content == nil {
			continue
		}
		text := plainText(ev.Content.Parts)
		if text == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, insert,
			text, ev.ID, sess.ID(), ev.Author, sess.AppName(), sess.UserID(),
			ev.Timestamp.UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("index event %s: %w", ev.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit memory tx: %w", err)
	}
	return nil
}

// SearchMemory returns up to topK indexed entries whose text matches the query,
// scoped to the request's (app, user). Queries of ≥3 runes use FTS5 trigram
// MATCH (ranked by relevance); shorter queries fall back to a recency-ordered
// LIKE scan, since trigram cannot index fewer than 3 characters.
func (s *Search) SearchMemory(ctx context.Context, req *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	resp := &adkmemory.SearchResponse{}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return resp, nil
	}

	var (
		rows *sql.Rows
		err  error
	)
	if utf8.RuneCountInString(query) < 3 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT content, event_id, author, ts FROM memory_fts
			 WHERE app_name = ? AND user_id = ? AND content LIKE ? ESCAPE '\'
			 ORDER BY ts DESC LIMIT ?`,
			req.AppName, req.UserID, "%"+escapeLike(query)+"%", s.topK)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT content, event_id, author, ts FROM memory_fts
			 WHERE app_name = ? AND user_id = ? AND content MATCH ?
			 ORDER BY rank LIMIT ?`,
			req.AppName, req.UserID, ftsMatchQuery(query), s.topK)
	}
	if err != nil {
		return nil, fmt.Errorf("search memory: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var content, eventID, author, ts string
		if err := rows.Scan(&content, &eventID, &author, &ts); err != nil {
			return nil, fmt.Errorf("scan memory row: %w", err)
		}
		when, _ := time.Parse(time.RFC3339, ts)
		resp.Memories = append(resp.Memories, adkmemory.Entry{
			ID:        eventID,
			Content:   &genai.Content{Role: roleFor(author), Parts: []*genai.Part{{Text: content}}},
			Author:    author,
			Timestamp: when,
		})
	}
	return resp, rows.Err()
}

// plainText joins the user-visible text of an event's parts, dropping reasoning
// (Thought) parts so internal chain-of-thought is never indexed.
func plainText(parts []*genai.Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p == nil || p.Thought || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.Text)
	}
	return strings.TrimSpace(b.String())
}

// ftsMatchQuery wraps the query as a single FTS5 string literal so arbitrary
// punctuation (which would otherwise be parsed as FTS5 query operators) is
// treated as a substring to match. Embedded double quotes are doubled per FTS5
// syntax.
func ftsMatchQuery(q string) string {
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
}

// escapeLike escapes the LIKE wildcards and the escape character itself, paired
// with `ESCAPE '\'` in the query.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// roleFor maps an event author to a genai content role. ADK authors are agent
// names or "user"; anything that isn't the user is treated as model output.
func roleFor(author string) string {
	if strings.EqualFold(author, "user") {
		return genai.RoleUser
	}
	return genai.RoleModel
}
