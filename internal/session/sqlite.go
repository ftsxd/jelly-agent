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
func New(ref string) (adksession.Service, error) {
	if ref == "" {
		p, err := DefaultDBPath()
		if err != nil {
			return nil, err
		}
		ref = p
	}
	kind, err := storage.KindOf(ref)
	if err != nil {
		return nil, err
	}
	var dialector gorm.Dialector
	switch kind {
	case storage.KindPostgres:
		dialector = postgres.Open(ref)
	default:
		if err := os.MkdirAll(filepath.Dir(ref), 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
		dialector = sqlite.Open(ref)
	}

	// Silence GORM's default logger: it logs ErrRecordNotFound for the
	// expected-empty app/user state lookups, which would pollute CLI output.
	svc, err := database.NewSessionService(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		// Not ref: a PostgreSQL URL may carry a password.
		return nil, fmt.Errorf("open session db (%s): %w", kind, err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		return nil, fmt.Errorf("migrate session db: %w", err)
	}
	return svc, nil
}
