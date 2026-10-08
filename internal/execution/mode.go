package execution

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// A session can be switched to "ask for every command": read-only queries
// that the rules would run directly, and classes covered by session grants,
// then wait for a person like any other command — until switched back. It is
// the visible off switch for everything that otherwise runs without asking.
const sessionModeSchema = `CREATE TABLE IF NOT EXISTS execution_session_modes (
 session_id TEXT PRIMARY KEY, strict BIGINT NOT NULL DEFAULT 0,
 updated_ms BIGINT NOT NULL, updated_by TEXT NOT NULL DEFAULT ''
);`

// StrictReason is what an approval card says when a command that would have
// run on its own is waiting only because the session asks for everything.
const StrictReason = "本会话已设为逐条审批：只读查询也需要你确认后执行"

// ErrModeUnavailable means the store has no session-mode table: a PostgreSQL
// deployment that has not applied migrations/postgres/0005_execution_session_modes.sql.
var ErrModeUnavailable = errors.New("逐条审批模式需要先执行 migrations/postgres/0005_execution_session_modes.sql")

// SessionMode reports whether session asks for every command, and whether the
// store can record the choice at all. A store error other than a missing
// table reads as strict: the person asked to be asked, and a lost answer must
// not quietly run commands they wanted to see first.
func (s Approvals) SessionMode(ctx context.Context, session string) (strict, available bool) {
	if session == "" {
		return false, true
	}
	var v int64
	err := s.DB.QueryRowContext(ctx, `SELECT strict FROM execution_session_modes WHERE session_id=?`, session).Scan(&v)
	switch {
	case err == nil:
		return v != 0, true
	case errors.Is(err, sql.ErrNoRows):
		return false, true
	case errors.Is(modeStoreError(err), ErrModeUnavailable):
		return false, false
	default:
		return true, true
	}
}

// Strict is SessionMode's first answer, for the paths that only gate.
func (s Approvals) Strict(ctx context.Context, session string) bool {
	strict, _ := s.SessionMode(ctx, session)
	return strict
}

// SetStrict switches session to asking for every command, or back.
func (s Approvals) SetStrict(ctx context.Context, session string, strict bool, actor string) error {
	v := int64(0)
	if strict {
		v = 1
	}
	err := s.DB.InTx(ctx, func(tx *storage.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM execution_session_modes WHERE session_id=?`, session); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO execution_session_modes (session_id,strict,updated_ms,updated_by) VALUES (?,?,?,?)`,
			session, v, time.Now().UnixMilli(), actor)
		return err
	})
	return modeStoreError(err)
}

func modeStoreError(err error) error {
	if err != nil && isMissingTable(err, "execution_session_modes") {
		return ErrModeUnavailable
	}
	return err
}

// ensureSessionModeSchema is best effort, like the grant table: without it
// the switch is simply not offered.
func ensureSessionModeSchema(db *storage.DB) error {
	return storage.ApplySchema(db, sessionModeSchema, "execution_session_modes")
}
