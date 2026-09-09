package storage

import "strings"

// dialect is everything one database does differently from another. There is
// exactly one implementation today; a second goes in a file beside sqlite.go
// and nothing outside this package changes, which is the point of the package.
type dialect interface {
	// driver names the registered database/sql driver.
	driver() string
	// prepare validates or prepares the reference before database/sql sees it.
	// SQLite creates a parent directory for its file; PostgreSQL has none.
	prepare(ref string) error
	// rebind converts a query written with `?` into this dialect's placeholder
	// syntax. Queries are written with `?` everywhere because that is what the
	// repo already had; only this function knows the difference.
	rebind(query string) string
	// configure applies pool policy and per-connection setup.
	configure(*sqlDB) error
	// hasColumn asks whether a table already has a column.
	hasColumn(db *DB, table, column string) (bool, error)
	// isUniqueViolation classifies a uniqueness clash by error code.
	isUniqueViolation(err error) bool
	// createsOwnSchema says whether a store may create its tables on open, or
	// whether the schema belongs to a migration the operator runs.
	createsOwnSchema() bool
	// hasTable asks whether a table exists, for the dialects that only check.
	hasTable(db *DB, table string) (bool, error)
	// columns lists a table's column names, for comparing one dialect's
	// schema against another's.
	columns(db *DB, table string) ([]string, error)
	// columnTypes maps a table's column names to this dialect's type names.
	columnTypes(db *DB, table string) (map[string]string, error)
	// isMissingTable classifies "that table does not exist".
	isMissingTable(err error) bool
	// epochSeconds renders a timestamp column as whole seconds since the
	// epoch. SQLite spells it strftime('%s', c); nothing else has strftime.
	epochSeconds(column string) string
}

// scanPlaceholders walks q and calls emit for each `?` that is really a
// placeholder, copying everything else through.
//
// The skipping is not decoration. A `?` inside a string literal, a quoted
// identifier or a comment is data, and renumbering it corrupts the query —
// silently, because the result is still valid SQL. This has to be right before
// any dialect uses it, so it is written once here and tested on its own.
//
// Handled: '…' with ” escapes, "…" with "" escapes, -- to end of line,
// /* … */ (not nested, matching SQL's own rule).
func scanPlaceholders(q string, emit func(*builder, int)) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); {
		switch c := q[i]; {
		case c == '?':
			n++
			emit(&b, n)
			i++
		case c == '\'' || c == '"':
			// A quoted run ends at the matching quote.
			quote := c
			b.WriteByte(c)
			i++
			for i < len(q) {
				b.WriteByte(q[i])
				if q[i] == quote {
					i++
					break
				}
				i++
			}
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			for i < len(q) && q[i] != '\n' {
				b.WriteByte(q[i])
				i++
			}
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			b.WriteString("/*")
			i += 2
			for i < len(q) {
				if q[i] == '*' && i+1 < len(q) && q[i+1] == '/' {
					b.WriteString("*/")
					i += 2
					break
				}
				b.WriteByte(q[i])
				i++
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// builder and itoa keep the scanner's signature free of imports that would
// otherwise have to be repeated by every dialect that uses it.
type builder = strings.Builder

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
