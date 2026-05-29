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
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
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

// NewSQLite opens a SQLite-backed session.Service at path, creating parent
// directories and running schema auto-migration. Pass an empty path to use
// DefaultDBPath.
func NewSQLite(path string) (adksession.Service, error) {
	if path == "" {
		p, err := DefaultDBPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	// Silence GORM's default logger: it logs ErrRecordNotFound for the
	// expected-empty app/user state lookups, which would pollute CLI output.
	svc, err := database.NewSessionService(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open session db %s: %w", path, err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		return nil, fmt.Errorf("migrate session db: %w", err)
	}
	return svc, nil
}
