package toolreg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// declSchema is the SQLite half of these two tables. PostgreSQL's is
// migrations/postgres/0001_init.sql, and engine.TestSQLiteAndPostgresSchemasAgree
// compares them so the two definitions cannot drift apart.
//
// changed_at and updated_at are DATETIME rather than TEXT for the same reason
// schedule_runs uses it: the Go side passes and scans a time.Time, and the
// driver round-trips that through DATETIME. Declared TEXT, the write succeeds
// and the read fails.
const declSchema = `
CREATE TABLE IF NOT EXISTS tool_decls (
	server        TEXT NOT NULL DEFAULT '',
	name          TEXT NOT NULL,
	-- Every value column is nullable, and that is the design: NULL means this
	-- layer declares nothing here, which is a different statement from
	-- declaring it empty and is what a YAML zero value could never express.
	description   TEXT,
	use_cases     TEXT,
	examples      TEXT,
	anti_examples TEXT,
	suites        TEXT,
	produces      TEXT,
	side_effect   TEXT,
	updated_at    DATETIME NOT NULL,
	updated_by    TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (server, name)
);
CREATE TABLE IF NOT EXISTS tool_decl_log (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	server     TEXT NOT NULL DEFAULT '',
	name       TEXT NOT NULL,
	op         TEXT NOT NULL,
	snapshot   TEXT,
	changed_at DATETIME NOT NULL,
	changed_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tool_decl_log_tool ON tool_decl_log(server, name, id);
`

// EnsureSchema creates the declaration tables. Called once when the shared
// handle is opened, like every other store's.
func EnsureSchema(db *storage.DB) error {
	return storage.ApplySchema(db, declSchema, "tool_decls", "tool_decl_log")
}

// Decl is one console declaration, field by field.
//
// Every field is a pointer, and that is the whole design. A save carries the
// fields somebody changed and says nothing about the others; nil means "leave
// it alone", which is a different statement from "set it to empty" and could
// not be told apart when this was a YAML file of zero values. Clearing a field
// is a non-nil pointer to the empty value.
type Decl struct {
	Server, Name string

	Description  *string
	UseCases     *[]string
	Examples     *[]string
	AntiExamples *[]string
	Suites       *[]string
	Produces     *string
	SideEffect   *string
}

// SaveDecl applies a patch and records it, as one transaction.
//
// The two go together on purpose. The log is what was missing when a save
// cleared a field and nobody could find out what had been there; a log that
// can disagree with the table it describes would not have helped.
//
// changedBy is recorded as given and may be empty — an unattributed change is
// still worth having, and inventing an attribution would be worse.
func SaveDecl(ctx context.Context, db *storage.DB, d Decl, changedBy string) error {
	if d.Name == "" {
		return fmt.Errorf("toolreg: 保存声明需要工具名")
	}
	now := time.Now().UTC()
	return db.InTx(ctx, func(tx *storage.Tx) error {
		cur, err := readDecl(ctx, tx, d.Server, d.Name)
		if err != nil {
			return err
		}
		next := patch(cur, d)

		if isEmptyDecl(next) {
			// Nothing left declared. The row goes rather than staying as a
			// row of NULLs: "not declared" and "declared as nothing" look the
			// same to every reader, and only the first is true.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM tool_decls WHERE server=? AND name=?`, d.Server, d.Name); err != nil {
				return fmt.Errorf("toolreg: delete decl: %w", err)
			}
			return appendLog(ctx, tx, d.Server, d.Name, "delete", nil, now, changedBy)
		}

		if err := upsertDecl(ctx, tx, next, now, changedBy); err != nil {
			return err
		}
		return appendLog(ctx, tx, d.Server, d.Name, "upsert", &next, now, changedBy)
	})
}

// storedDecl is a row as it sits in the table: every column nullable.
type storedDecl struct {
	Server, Name                             string
	Description, Produces, SideEffect        *string
	UseCases, Examples, AntiExamples, Suites *[]string
}

func readDecl(ctx context.Context, tx *storage.Tx, server, name string) (storedDecl, error) {
	out := storedDecl{Server: server, Name: name}
	var uc, ex, ae, su *string
	err := tx.QueryRowContext(ctx, `
		SELECT description, use_cases, examples, anti_examples, suites, produces, side_effect
		FROM tool_decls WHERE server=? AND name=?`, server, name).
		Scan(&out.Description, &uc, &ex, &ae, &su, &out.Produces, &out.SideEffect)
	if err != nil {
		if isNoRows(err) {
			return out, nil // nothing declared yet, which is a valid starting point
		}
		return out, fmt.Errorf("toolreg: read decl: %w", err)
	}
	for _, f := range []struct {
		raw *string
		dst **[]string
	}{{uc, &out.UseCases}, {ex, &out.Examples}, {ae, &out.AntiExamples}, {su, &out.Suites}} {
		if f.raw == nil {
			continue
		}
		var v []string
		if err := json.Unmarshal([]byte(*f.raw), &v); err != nil {
			return out, fmt.Errorf("toolreg: %s/%s 的列不是 JSON 数组: %w", server, name, err)
		}
		*f.dst = &v
	}
	return out, nil
}

// patch applies the non-nil fields of d over cur.
func patch(cur storedDecl, d Decl) storedDecl {
	if d.Description != nil {
		cur.Description = d.Description
	}
	if d.Produces != nil {
		cur.Produces = d.Produces
	}
	if d.SideEffect != nil {
		cur.SideEffect = d.SideEffect
	}
	if d.UseCases != nil {
		cur.UseCases = d.UseCases
	}
	if d.Examples != nil {
		cur.Examples = d.Examples
	}
	if d.AntiExamples != nil {
		cur.AntiExamples = d.AntiExamples
	}
	if d.Suites != nil {
		cur.Suites = d.Suites
	}
	return cur
}

// isEmptyDecl reports whether a row would declare nothing at all.
//
// An empty string and an empty list count as nothing, because that is how the
// console says "I no longer want to override this" — a cleared textarea.
func isEmptyDecl(d storedDecl) bool {
	for _, s := range []*string{d.Description, d.Produces, d.SideEffect} {
		if s != nil && *s != "" {
			return false
		}
	}
	for _, l := range []*[]string{d.UseCases, d.Examples, d.AntiExamples, d.Suites} {
		if l != nil && len(*l) > 0 {
			return false
		}
	}
	return true
}

func upsertDecl(ctx context.Context, tx *storage.Tx, d storedDecl, now time.Time, by string) error {
	uc, ex, ae, su := jsonOrNil(d.UseCases), jsonOrNil(d.Examples),
		jsonOrNil(d.AntiExamples), jsonOrNil(d.Suites)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO tool_decls
			(server, name, description, use_cases, examples, anti_examples,
			 suites, produces, side_effect, updated_at, updated_by)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (server, name) DO UPDATE SET
			description=excluded.description, use_cases=excluded.use_cases,
			examples=excluded.examples, anti_examples=excluded.anti_examples,
			suites=excluded.suites, produces=excluded.produces,
			side_effect=excluded.side_effect,
			updated_at=excluded.updated_at, updated_by=excluded.updated_by`,
		d.Server, d.Name, emptyToNil(d.Description), uc, ex, ae, su,
		emptyToNil(d.Produces), emptyToNil(d.SideEffect), now, by)
	if err != nil {
		return fmt.Errorf("toolreg: upsert decl: %w", err)
	}
	return nil
}

// appendLog records the change and, in doing so, bumps the version every
// process polls. One statement serves both, so a change can never be applied
// without being announced.
func appendLog(ctx context.Context, tx *storage.Tx, server, name, op string,
	after *storedDecl, now time.Time, by string,
) error {
	var snapshot *string
	if after != nil {
		// The whole declaration as it now stands, not a diff. Answering "what
		// was it at the time" from a snapshot is one row; from diffs it is a
		// replay from the beginning.
		b, err := json.Marshal(after)
		if err != nil {
			return fmt.Errorf("toolreg: encode snapshot: %w", err)
		}
		s := string(b)
		snapshot = &s
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO tool_decl_log (server, name, op, snapshot, changed_at, changed_by)
		VALUES (?,?,?,?,?,?)`, server, name, op, snapshot, now, by)
	if err != nil {
		return fmt.Errorf("toolreg: append decl log: %w", err)
	}
	return nil
}

// DeclHistory is one recorded change.
type DeclHistory struct {
	ID        int64             `json:"id"`
	Server    string            `json:"server,omitempty"`
	Name      string            `json:"name"`
	Op        string            `json:"op"`
	Snapshot  *ops.ToolMetadata `json:"snapshot,omitempty"`
	ChangedAt time.Time         `json:"changed_at"`
	ChangedBy string            `json:"changed_by,omitempty"`
}

// History returns the most recent changes to one tool, newest first.
func History(ctx context.Context, db *storage.DB, server, name string, limit int) ([]DeclHistory, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, server, name, op, snapshot, changed_at, changed_by
		FROM tool_decl_log WHERE server=? AND name=? ORDER BY id DESC LIMIT ?`,
		server, name, limit)
	if err != nil {
		if storage.IsMissingTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("toolreg: read decl log: %w", err)
	}
	defer rows.Close()
	var out []DeclHistory
	for rows.Next() {
		var h DeclHistory
		var snap *string
		if err := rows.Scan(&h.ID, &h.Server, &h.Name, &h.Op, &snap, &h.ChangedAt, &h.ChangedBy); err != nil {
			return nil, err
		}
		if snap != nil {
			var sd storedDecl
			if err := json.Unmarshal([]byte(*snap), &sd); err == nil {
				m := sd.toMetadata()
				h.Snapshot = &m
			}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (d storedDecl) toMetadata() ops.ToolMetadata {
	m := ops.ToolMetadata{Server: d.Server, Name: d.Name}
	if d.Description != nil {
		m.Description = *d.Description
	}
	if d.Produces != nil {
		m.Produces = ops.EvidenceKind(*d.Produces)
	}
	if d.SideEffect != nil {
		m.SideEffect = ops.SideEffectLevel(*d.SideEffect)
	}
	for _, f := range []struct {
		src *[]string
		dst *[]string
	}{{d.UseCases, &m.UseCases}, {d.Examples, &m.Examples},
		{d.AntiExamples, &m.AntiExamples}, {d.Suites, &m.Suites}} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
	return m
}

func jsonOrNil(v *[]string) *string {
	if v == nil || len(*v) == 0 {
		return nil
	}
	b, err := json.Marshal(*v)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

func emptyToNil(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

// isNoRows is sql.ErrNoRows without this file importing database/sql for one
// comparison — the guard test in internal/storage bans the native types, and
// the error value is reachable through the same package that owns them.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
