package storage

import (
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
)

// Everything PostgreSQL-specific. Its counterpart is sqlite.go; between them
// they are the only files in the process that name a database.

type postgresDialect struct{}

func (postgresDialect) driver() string { return "pgx" }

// prepare has nothing to do: there is no directory to create, and validating
// the URL is database/sql's job — it reports a malformed DSN better than a
// second parser here would.
func (postgresDialect) prepare(string) error { return nil }

// rebind turns `?` into $1, $2, … — PostgreSQL numbers its placeholders.
//
// Queries throughout the repo are written with `?` because that is what they
// already were; this is the only place the difference exists. The scanning is
// the careful part and lives in dialect.go: a `?` inside a string literal, a
// quoted identifier or a comment is data, and renumbering it yields a query
// that is still valid SQL and quietly wrong.
func (postgresDialect) rebind(q string) string {
	if !strings.Contains(q, "?") {
		return q // most DDL; skip the scan entirely
	}
	return scanPlaceholders(q, func(b *builder, n int) {
		b.WriteByte('$')
		b.WriteString(itoa(n))
	})
}

// postgresConnLimit caps one handle's pool.
//
// Deliberately small, because the process does not hold one pool. Seven stores
// each open their own handle to the same database, so the server's connection
// count is this number times seven — against a server whose own default
// max_connections is 100. Four keeps that at 28, with room left for psql and
// for a second instance of this process.
//
// The number is a symptom rather than a design. Handles should be shared per
// DSN, and four of the seven stores make it worse by opening one per call
// instead of holding it. Both are worth fixing; neither is a reason to leave
// the cap unset in the meantime.
const postgresConnLimit = 4

// postgresConnLifetime is how long one pooled connection may live. Long enough
// that a busy handle is not reconnecting, short enough that a failover or a
// restarted server does not leave this process holding connections to
// something that is gone.
const postgresConnLifetime = 30 * time.Minute

func (postgresDialect) configure(db *sqlDB) error {
	db.SetMaxOpenConns(postgresConnLimit)
	db.SetMaxIdleConns(postgresConnLimit)
	db.SetConnMaxLifetime(postgresConnLifetime)
	return nil
}

// hasColumn asks the catalogue. SQLite's pragma_table_info has no equivalent
// here; information_schema is the standard spelling and PostgreSQL implements
// it faithfully.
//
// current_schema() rather than a literal 'public': which schema the tables
// live in is a deployment's choice, and a migration run into another one
// should still be seen.
func (postgresDialect) hasColumn(db *DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRow(`
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
		table, column).Scan(&n)
	return n > 0, err
}

// postgresUniqueViolation is SQLSTATE 23505, verified against PostgreSQL 16.6.
const postgresUniqueViolation = "23505"

func (postgresDialect) isUniqueViolation(err error) bool {
	var e *pgconn.PgError
	return errors.As(err, &e) && e.Code == postgresUniqueViolation
}
