// Package session provides jelly-agent's persistent session storage. It wraps
// ADK-Go's GORM-backed session.Service with a pure-Go SQLite driver
// (modernc.org/sqlite via glebarez), so persistence needs no CGO and keeps the
// single-binary deployment goal.
package session

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	adksession "google.golang.org/adk/session"
	"google.golang.org/adk/session/database"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// DefaultDBPath returns the default state.db path under ~/.jelly-agent. The
// session store and the memory FTS5 index (see PLAN §10) share this file.
func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".jelly-agent", "state.db"), nil
}

// New opens the persistent session store ADK writes through.
//
// ref selects the database the same way storage.Open does: a postgres:// URL
// means PostgreSQL, anything else is a SQLite file whose parent directory is
// created. Empty means DefaultDBPath.
//
// It cannot go through storage.DB, because ADK's session service takes a GORM
// dialector rather than a database/sql handle — so this is the one place
// outside internal/storage that names a database. storage.KindOf makes the
// choice, so a reference this accepts is one Open accepts too: the two halves
// of the same database cannot end up pointed at different places.
//
// The tables here belong to ADK. AutoMigrate creates them, and there is
// deliberately no hand-written DDL for them anywhere in this repo — see
// session.EnsureSchema.
func New(ref string) (adksession.Service, func() error, error) {
	if ref == "" {
		p, err := DefaultDBPath()
		if err != nil {
			return nil, nil, err
		}
		ref = p
	}
	// The pool is this repo's, not GORM's.
	//
	// Handing the driver a DSN lets it open its own connection with no limit
	// set — one more unbounded pool against a server whose own default
	// max_connections is 100, and one nothing can close, because ADK's Service
	// interface has no Close. Opening it here means it obeys the same policy
	// as every other handle and the caller gets something to release.
	db, err := storage.Open(ref)
	if err != nil {
		return nil, nil, err
	}
	var dialector gorm.Dialector
	switch db.Kind() {
	case storage.KindPostgres:
		dialector = postgres.New(postgres.Config{Conn: db.Native()})
	default:
		dialector = &sqlite.Dialector{Conn: db.Native()}
	}

	// Silence GORM's default logger: it logs ErrRecordNotFound for the
	// expected-empty app/user state lookups, which would pollute CLI output.
	svc, err := database.NewSessionService(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		db.Close()
		// Not ref: a PostgreSQL URL may carry a password.
		return nil, nil, fmt.Errorf("open session db (%s): %w", db.Kind(), err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("migrate session db: %w", err)
	}
	// After AutoMigrate, because that is when the tables exist. See
	// EnsureIndexes: ADK declares no index beyond its composite primary keys,
	// and both queries this repo runs against those tables scan without them.
	if err := EnsureIndexes(db); err != nil {
		db.Close()
		return nil, nil, err
	}
	return svc, db.Close, nil
}
