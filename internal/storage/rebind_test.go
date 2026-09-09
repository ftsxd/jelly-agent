package storage

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

// A stand-in for the dialect that renumbers, so the scanner can be tested
// before one exists. What it produces is PostgreSQL's syntax, which is what
// the real one will produce.
func numbered(q string) string {
	return scanPlaceholders(q, func(b *builder, n int) { b.WriteString("$" + itoa(n)) })
}

func TestScanPlaceholdersNumbersInOrder(t *testing.T) {
	got := numbered(`SELECT a FROM t WHERE x=? AND y=? AND z IN (?,?,?)`)
	want := `SELECT a FROM t WHERE x=$1 AND y=$2 AND z IN ($3,$4,$5)`
	if got != want {
		t.Errorf("\ngot  %s\nwant %s", got, want)
	}
}

func TestScanPlaceholdersPastTen(t *testing.T) {
	// $10 is two digits; a renumberer that assumed one would produce $1 0.
	q := ""
	for range 12 {
		q += "?,"
	}
	got := numbered("IN (" + q[:len(q)-1] + ")")
	want := "IN ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)"
	if got != want {
		t.Errorf("\ngot  %s\nwant %s", got, want)
	}
}

// A `?` that is not a placeholder is data. Renumbering it corrupts the query
// and the result is still valid SQL, so nothing complains — the row is just
// wrong, or the comment now says something else.
func TestScanPlaceholdersLeavesNonPlaceholdersAlone(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"字符串字面量里的问号",
			`SELECT ? WHERE note = 'why? because'`,
			`SELECT $1 WHERE note = 'why? because'`,
		},
		{
			"字符串里转义的单引号不结束字符串",
			`SELECT ? WHERE s = 'it''s a ? here' AND x = ?`,
			`SELECT $1 WHERE s = 'it''s a ? here' AND x = $2`,
		},
		{
			"双引号标识符里的问号",
			`SELECT "odd?col" FROM t WHERE x = ?`,
			`SELECT "odd?col" FROM t WHERE x = $1`,
		},
		{
			"标识符里转义的双引号",
			`SELECT "a""b?c" FROM t WHERE x = ?`,
			`SELECT "a""b?c" FROM t WHERE x = $1`,
		},
		{
			"行注释里的问号",
			"SELECT ? -- really? yes\nAND y = ?",
			"SELECT $1 -- really? yes\nAND y = $2",
		},
		{
			"块注释里的问号",
			`SELECT ? /* who? nobody */ AND y = ?`,
			`SELECT $1 /* who? nobody */ AND y = $2`,
		},
		{
			"注释未闭合时不吞掉后面的语句",
			`SELECT ? /* unterminated`,
			`SELECT $1 /* unterminated`,
		},
		{
			"字符串未闭合时同样不吞",
			`SELECT ? WHERE s = 'unterminated ?`,
			`SELECT $1 WHERE s = 'unterminated ?`,
		},
		{
			"一个占位符都没有",
			`SELECT count(*) FROM t`,
			`SELECT count(*) FROM t`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := numbered(tc.in); got != tc.want {
				t.Errorf("\ngot  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// SQLite's rebind is the identity, and it has to be exactly that — the
// mechanical migration of every call site onto the wrapper relies on nothing
// changing.
func TestSQLiteRebindIsTheIdentity(t *testing.T) {
	for _, q := range []string{
		`SELECT a FROM t WHERE x=? AND y=?`,
		`SELECT ? WHERE s = 'why? ' -- and? \n`,
		``,
	} {
		if got := (sqliteDialect{}).rebind(q); got != q {
			t.Errorf("rebind changed the query:\ngot  %q\nwant %q", got, q)
		}
	}
}

// recordingDialect is SQLite plus a note of every query it was asked to
// rebind, so a test can see whether a statement went through the seam at all.
type recordingDialect struct {
	sqliteDialect
	seen *[]string
}

func (r recordingDialect) rebind(q string) string {
	*r.seen = append(*r.seen, q)
	return r.sqliteDialect.rebind(q)
}

// A Tx must rebind through its DB's dialect, or the same statement means one
// thing outside a transaction and another inside it — and since SQLite's
// rebind is the identity, a Tx that skipped the seam entirely would behave
// identically here and fail only against PostgreSQL.
//
// So this drives the real path (Open → InTx → Tx.Exec) and asserts the
// statement was seen, rather than comparing two dialects a test constructed.
func TestTxRebindsThroughItsHandlesDialect(t *testing.T) {
	var seen []string
	db, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.dialect = recordingDialect{seen: &seen}

	if _, err := db.Exec(`CREATE TABLE t (a TEXT)`); err != nil {
		t.Fatal(err)
	}
	const inTx = `INSERT INTO t (a) VALUES (?)`
	if err := db.InTx(context.Background(), func(tx *Tx) error {
		_, err := tx.Exec(inTx, "x")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	const inRead = `SELECT a FROM t WHERE a = ?`
	if err := db.InReadTx(context.Background(), func(tx *Tx) error {
		return tx.QueryRow(inRead, "x").Scan(new(string))
	}); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{inTx, inRead} {
		if !slices.Contains(seen, want) {
			t.Errorf("statement never reached the dialect: %s\nseen: %v", want, seen)
		}
	}
}
