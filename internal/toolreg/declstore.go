package toolreg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
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
		return saveDecl(ctx, tx, d, now, changedBy)
	})
}

// saveDecl is SaveDecl's body, inside a transaction somebody else owns.
//
// Split out for the import, which applies a file's worth of declarations and
// has to do it as one unit — see ImportConsoleFile.
func saveDecl(ctx context.Context, tx *storage.Tx, d Decl, now time.Time, changedBy string) error {
	// The row is locked before it is read, and that is the whole point.
	//
	// Read-modify-write without it is the bug this table was created to
	// fix, reintroduced: under READ COMMITTED two saves for the same tool
	// both read the row as it was, each applies its own field, and the
	// second write erases the first one's. Demonstrated on PostgreSQL —
	// A sets suites, B sets produces, and suites is gone.
	//
	// A lock rather than a compare-and-set retry because these are
	// patches, not proposals: the second save's field is still wanted, it
	// just has to be applied to what the first one left behind. Waiting a
	// few milliseconds for that is the correct outcome, and there is at
	// most one console.
	if err := lockDecl(ctx, tx, d.Server, d.Name); err != nil {
		return err
	}
	cur, err := readDecl(ctx, tx, d.Server, d.Name)
	if err != nil {
		return err
	}
	afterRead() // a seam, so a test can hold one save between its read and its write
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
}

// storedDecl is a row as it sits in the table: every column nullable.
type storedDecl struct {
	Server, Name                             string
	Description, Produces, SideEffect        *string
	UseCases, Examples, AntiExamples, Suites *[]string
}

// lockDecl serialises everything that touches one tool's declaration.
//
// A transaction-scoped key lock rather than SELECT … FOR UPDATE, and the
// difference is the whole bug. FOR UPDATE locks a row that exists; a tool
// nobody has declared yet has none, so two first-saves both lock nothing,
// both read nothing, and both write a row built from nothing. The second
// insert conflicts on the primary key and its DO UPDATE assigns *its own*
// columns — excluded.*, not a merge — so whatever the first one declared is
// erased. Demonstrated on PostgreSQL: A saves suites, B saves produces, and
// the row ends up with only produces.
//
// The key stands in for the identity being created, so it exists before the
// row does. Held to the end of the transaction, so there is nothing to
// release and no window between taking it and committing.
//
// SQLite has no such lock and needs none: one writer at a time, and this
// package's handle allows one connection.
func lockDecl(ctx context.Context, tx *storage.Tx, server, name string) error {
	if err := tx.LockKey(ctx, storage.KeyOf("tool_decls", server, name)); err != nil {
		return fmt.Errorf("toolreg: lock decl %s/%s: %w", server, name, err)
	}
	return nil
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

// afterRead is the point between reading the row and writing it back.
//
// The window this whole function is about: without the row lock above, a
// second save slipping in here reads the row as it was and then erases what
// the first one wrote. Two goroutines almost never land in it on their own —
// the transaction is microseconds long — so the test that proves the lock
// works has to be able to hold one save open, and a mutation that removes the
// lock has to actually fail.
//
// A variable rather than a parameter because every caller would otherwise
// pass nil, and a nil check at every call site is more to read than this.
var afterRead = func() {}

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

// ImportConsoleFile moves an existing console.yaml into the table, once.
//
// The console's layer used to be that file. A deployment upgrading across
// this change has one, and it holds decisions somebody made — which tools are
// PromQL, what a tool is for — that would otherwise silently stop applying.
//
// Runs only when the table is empty, so it cannot overwrite anything decided
// since. The file is renamed rather than deleted: an import that got something
// wrong should be recoverable by a person, and the rename is also what stops
// the next start from importing it again.
//
// All of it in one transaction, and that is not tidiness. Saved one at a time,
// a failure partway — a crash, a declaration the table refuses — leaves some
// rows in and the rest out, and the emptiness check then makes that permanent:
// the next start sees a non-empty table, skips the import, and the
// declarations that never made it are gone while the file still sits there
// looking un-imported. Either the whole file is in the table or none of it is.
//
// Returns how many declarations moved. A missing file is zero and no error —
// that is a fresh deployment, which is the common case.
func ImportConsoleFile(ctx context.Context, db *storage.DB, path string) (int, error) {
	var existing int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tool_decls`).Scan(&existing); err != nil {
		return 0, fmt.Errorf("toolreg: count decls: %w", err)
	}
	if existing > 0 {
		return 0, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("toolreg: read %s: %w", path, err)
	}
	var f metadataFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return 0, fmt.Errorf("toolreg: parse %s: %w", path, err)
	}

	n := 0
	if err := db.InTx(ctx, func(tx *storage.Tx) error {
		// Counted again inside the transaction. The check above is a fast
		// path that avoids reading the file on every start; this one is the
		// one that decides, and it sees a table that cannot change underneath
		// it while the rows go in.
		//
		// Two processes starting at the same moment on a fresh database can
		// both find it empty and both import. That is harmless rather than
		// prevented: the declarations are the same ones, the primary key
		// merges them, and the only trace is a duplicate pair of audit
		// entries. Serialising it would need a lock held across a file read
		// for a case that happens once in a deployment's life.
		n = 0
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tool_decls`).Scan(&existing); err != nil {
			return fmt.Errorf("toolreg: count decls: %w", err)
		}
		if existing > 0 {
			return nil
		}
		now := time.Now().UTC()
		for _, m := range f.Tools {
			if m.Name == "" {
				continue
			}
			// Every field is carried, empty ones included, and they still do not
			// become overrides — emptyToNil stores an empty value as NULL, which
			// is how the table says "not declared". An earlier version filtered
			// them here as well; no input could tell the two apart, so the filter
			// went rather than staying as a branch nothing exercises.
			//
			// A YAML zero value meant "not declared", and this is where those two
			// representations meet.
			produces, effect := string(m.Produces), string(m.SideEffect)
			d := Decl{
				Server: m.Server, Name: m.Name,
				Description:  &m.Description,
				Produces:     &produces,
				SideEffect:   &effect,
				UseCases:     &m.UseCases,
				Examples:     &m.Examples,
				AntiExamples: &m.AntiExamples,
				Suites:       &m.Suites,
			}
			if err := saveDecl(ctx, tx, d, now, "import:"+filepath.Base(path)); err != nil {
				return fmt.Errorf("toolreg: import %s/%s: %w", m.Server, m.Name, err)
			}
			n++
		}
		return nil
	}); err != nil {
		// Nothing was written: the transaction rolled back, so a later start
		// finds the table still empty and the file still in place, and tries
		// again.
		return 0, err
	}

	if err := os.Rename(path, path+".imported"); err != nil {
		// The rows are in. Reporting a failure here would make a caller retry
		// an import that already happened — and the count check above would
		// then make the retry a no-op anyway. Worth a mention, not a failure.
		return n, fmt.Errorf("toolreg: 已导入 %d 条，但重命名 %s 失败（下次启动会跳过，因为表已非空）: %w",
			n, path, err)
	}
	return n, nil
}
