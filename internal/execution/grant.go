package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// A session grant is "don't ask again for this kind of command in this
// conversation": one approval that also covers later commands of the same
// class — same session, agent and profile — until it expires, is revoked, or
// the configuration, credentials or sandbox state it was given under change.
//
// It trades per-command review for fewer interruptions, so what counts as a
// class is deliberately narrow, and some commands never get one at all (see
// GrantClass).
const GrantTTL = 8 * time.Hour

const grantSchema = `CREATE TABLE IF NOT EXISTS execution_grants (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, agent TEXT NOT NULL, profile TEXT NOT NULL,
 class TEXT NOT NULL, config_hash TEXT NOT NULL, approval_id TEXT NOT NULL,
 created_by TEXT NOT NULL, created_ms BIGINT NOT NULL, expires_ms BIGINT NOT NULL,
 revoked_ms BIGINT NOT NULL DEFAULT 0, uses BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS execution_grants_session ON execution_grants(session_id, class);`

// ErrGrantUnavailable means the store has no grant table: a PostgreSQL
// deployment that has not applied migrations/postgres/0004_execution_grants.sql.
// Approvals keep working one by one.
var ErrGrantUnavailable = errors.New("会话内免审批需要先执行 migrations/postgres/0004_execution_grants.sql；单次审批不受影响")

type Grant struct {
	ID         string `json:"id"`
	SessionID  string `json:"session_id"`
	Agent      string `json:"agent"`
	Profile    string `json:"profile"`
	Class      string `json:"class"`
	ApprovalID string `json:"approval_id"`
	CreatedBy  string `json:"created_by"`
	CreatedMS  int64  `json:"created_ms"`
	ExpiresMS  int64  `json:"expires_ms"`
	Uses       int64  `json:"uses"`
}

// kubectl subcommands a grant may cover. Left out: those that run arbitrary
// code or open a channel (exec, run, debug, attach, cp, proxy, port-forward),
// those whose effect lives in a manifest rather than argv (apply, create,
// replace), and anything unknown — a plugin is somebody else's program.
var grantableKubectl = map[string]bool{"scale": true, "rollout": true, "label": true, "annotate": true, "cordon": true,
	"uncordon": true, "drain": true, "taint": true, "set": true, "autoscale": true, "patch": true}

// GrantClass is the class a session grant for command would cover, or ""
// when it must be approved one by one: anything but a single tccli or kubectl
// call (a general command's "same kind" is any code at all), a read that
// returns credentials, or a kubectl subcommand outside grantableKubectl.
func GrantClass(command string) string {
	parsed, err := Parse(command)
	if err != nil || len(parsed.Segments) != 1 {
		return ""
	}
	a := parsed.Segments[0]
	switch a[0] {
	case "tccli":
		if len(a) < 3 || !identifier.MatchString(a[1]) || !identifier.MatchString(a[2]) {
			return ""
		}
		for _, word := range sensitiveReads {
			if strings.Contains(strings.ToLower(a[2]), word) {
				return ""
			}
		}
		return "tccli " + a[1] + " " + a[2]
	case "kubectl":
		sub, next, ok := kubectlSubcommand(a)
		if !ok || !grantableKubectl[sub] {
			return ""
		}
		// "rollout restart" and "rollout undo" are different decisions; so are
		// "set image" and "set env".
		if sub == "rollout" || sub == "set" {
			if !identifier.MatchString(next) {
				return ""
			}
			return "kubectl " + sub + " " + next
		}
		return "kubectl " + sub
	}
	return ""
}

// grantClassFor is GrantClass for this agent and request under c: "" also
// when the call could not be approved at all, or would run unconfined — a
// grant must never extend a run without isolation past the one approved.
func (c Config) grantClassFor(agent string, req Request) string {
	class := GrantClass(req.Command)
	if class == "" {
		return ""
	}
	r := Runtime{Config: c}
	if r.Check(agent, req).Decision != Prompt || !r.ApprovalEnabled(agent, req.Profile) {
		return ""
	}
	for _, p := range c.ProfilesFor(agent) {
		if p.Name == req.Profile && p.AllowUnconfinedWithApproval && checkStrict(r.policy(p, req)) != nil {
			return ""
		}
	}
	return class
}

// GrantClassFor exposes grantClassFor to the server, which offers the option
// only where it would be honoured.
func (c Config) GrantClassFor(agent string, req Request) string { return c.grantClassFor(agent, req) }

// CreateGrant records a session grant for an approval a person just gave,
// under the configuration current at that moment.
func (s Approvals) CreateGrant(ctx context.Context, c Config, a Approval, actor string) (Grant, error) {
	var g Grant
	err := guardConfig(ctx, c, func(current Config) error {
		class := current.grantClassFor(a.Agent, a.Request)
		if class == "" {
			return fmt.Errorf("该命令不支持会话内免审批，只能逐条批准")
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		now := time.Now()
		g = Grant{ID: "grant_" + hex.EncodeToString(id[:]), SessionID: a.SessionID, Agent: a.Agent, Profile: a.Request.Profile,
			Class: class, ApprovalID: a.ID, CreatedBy: actor, CreatedMS: now.UnixMilli(), ExpiresMS: now.Add(GrantTTL).UnixMilli()}
		_, err := s.DB.ExecContext(ctx, `INSERT INTO execution_grants
		 (id,session_id,agent,profile,class,config_hash,approval_id,created_by,created_ms,expires_ms)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`, g.ID, g.SessionID, g.Agent, g.Profile, g.Class, configHash(current), g.ApprovalID, actor, g.CreatedMS, g.ExpiresMS)
		if err != nil {
			return grantStoreError(err)
		}
		return nil
	})
	return g, err
}

// FindGrant returns a live grant covering req for this agent and session.
// Any doubt — a store error, a config that moved, a missing table — reads as
// no grant, which only means a person is asked again.
func (s Approvals) FindGrant(ctx context.Context, c Config, agent, session string, req Request) (Grant, bool) {
	class := c.grantClassFor(agent, req)
	if class == "" || session == "" {
		return Grant{}, false
	}
	var g Grant
	err := guardConfig(ctx, c, func(current Config) error {
		hash := configHash(current)
		if hash != configHash(c) {
			return ErrApproval
		}
		return s.DB.QueryRowContext(ctx, `SELECT id,session_id,agent,profile,class,approval_id,created_by,created_ms,expires_ms,uses
		 FROM execution_grants WHERE session_id=? AND agent=? AND profile=? AND class=? AND config_hash=? AND revoked_ms=0 AND expires_ms>?
		 ORDER BY created_ms DESC LIMIT 1`, session, agent, req.Profile, class, hash, time.Now().UnixMilli()).
			Scan(&g.ID, &g.SessionID, &g.Agent, &g.Profile, &g.Class, &g.ApprovalID, &g.CreatedBy, &g.CreatedMS, &g.ExpiresMS, &g.Uses)
	})
	if err != nil {
		return Grant{}, false
	}
	_, _ = s.DB.ExecContext(ctx, `UPDATE execution_grants SET uses=uses+1 WHERE id=?`, g.ID)
	g.Uses++
	return g, true
}

// Grants lists a session's grants that are still in force under c.
func (s Approvals) Grants(ctx context.Context, session string, c Config) ([]Grant, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,session_id,agent,profile,class,approval_id,created_by,created_ms,expires_ms,uses
	 FROM execution_grants WHERE session_id=? AND config_hash=? AND revoked_ms=0 AND expires_ms>? ORDER BY created_ms DESC`,
		session, configHash(c), time.Now().UnixMilli())
	if err != nil {
		return nil, grantStoreError(err)
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.SessionID, &g.Agent, &g.Profile, &g.Class, &g.ApprovalID, &g.CreatedBy, &g.CreatedMS, &g.ExpiresMS, &g.Uses); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeGrant ends a grant now; later commands of its class are asked again.
func (s Approvals) RevokeGrant(ctx context.Context, session, id string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE execution_grants SET revoked_ms=? WHERE id=? AND session_id=? AND revoked_ms=0`,
		time.Now().UnixMilli(), id, session)
	if err != nil {
		return grantStoreError(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("授权不存在或已撤销")
	}
	return nil
}

func grantStoreError(err error) error {
	if msg := err.Error(); strings.Contains(msg, "execution_grants") && (strings.Contains(msg, "does not exist") || strings.Contains(msg, "no such table")) {
		return ErrGrantUnavailable
	}
	return err
}

// ensureGrantSchema is best effort: without the table, approvals still work
// one by one, so a PostgreSQL deployment that has not migrated yet must not
// fail to start over an optional convenience.
func ensureGrantSchema(db *storage.DB) error {
	return storage.ApplySchema(db, grantSchema, "execution_grants")
}
