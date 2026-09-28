package execution

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

const ApprovalTTL = 10 * time.Minute

var ErrApproval = errors.New("审批不存在、已处理、已过期，或命令/执行配置已改变；请重新发起请求")

const approvalSchema = `CREATE TABLE IF NOT EXISTS execution_approvals (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, agent TEXT NOT NULL,
 invocation_id TEXT NOT NULL, call_id TEXT NOT NULL, request_json TEXT NOT NULL,
 config_hash TEXT NOT NULL, origin_json TEXT NOT NULL, reason TEXT NOT NULL,
 state TEXT NOT NULL, created_ms BIGINT NOT NULL, expires_ms BIGINT NOT NULL,
 resolved_ms BIGINT NOT NULL DEFAULT 0, resolved_by TEXT NOT NULL DEFAULT '',
 exec_id TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS execution_approvals_session ON execution_approvals(session_id, created_ms);`

func EnsureApprovalSchema(db *storage.DB) error {
	return storage.ApplySchema(db, approvalSchema, "execution_approvals")
}

type Origin struct {
	Agent    string `json:"agent"`
	Provider string `json:"provider"`
}
type originKey struct{}
type approvalKey struct{}
type configGuardKey struct{}
type ConfigGuard func(func(Config) error) error
type approvalAuthority struct {
	id      string
	onStart func(string)
}

// Context authority is supplied by the authenticated server, never model args.
func WithOrigin(ctx context.Context, agent, provider string) context.Context {
	return context.WithValue(ctx, originKey{}, Origin{Agent: agent, Provider: provider})
}
func WithApproval(ctx context.Context, id string, onStart ...func(string)) context.Context {
	a := approvalAuthority{id: id}
	if len(onStart) > 0 {
		a.onStart = onStart[0]
	}
	return context.WithValue(ctx, approvalKey{}, a)
}

// WithConfigGuard lets the server serialize authorization with config reload.
// An in-flight request may retain its engine, but may not retain revoked grants.
func WithConfigGuard(ctx context.Context, guard ConfigGuard) context.Context {
	return context.WithValue(ctx, configGuardKey{}, guard)
}

func guardConfig(ctx context.Context, fallback Config, check func(Config) error) error {
	if guard, ok := ctx.Value(configGuardKey{}).(ConfigGuard); ok && guard != nil {
		return guard(check)
	}
	return check(fallback)
}

// ADK resumes a confirmed tool before emitting its first event. Notify the
// server of that invocation so status and task membership precede execution.
func ApprovalStarted(ctx context.Context, invocation string) {
	a, _ := ctx.Value(approvalKey{}).(approvalAuthority)
	if a.onStart != nil {
		a.onStart(invocation)
	}
}

type Approval struct {
	ID           string  `json:"id"`
	SessionID    string  `json:"session_id"`
	Agent        string  `json:"agent"`
	InvocationID string  `json:"invocation_id"`
	CallID       string  `json:"call_id"`
	Request      Request `json:"request"`
	ConfigHash   string  `json:"-"`
	Origin       Origin  `json:"origin"`
	Reason       string  `json:"reason"`
	State        string  `json:"state"`
	CreatedMS    int64   `json:"created_ms"`
	ExpiresMS    int64   `json:"expires_ms"`
	ResolvedMS   int64   `json:"resolved_ms,omitempty"`
	ResolvedBy   string  `json:"resolved_by,omitempty"`
	ExecID       string  `json:"exec_id,omitempty"`
	Outcome      string  `json:"outcome,omitempty"`
}
type Approvals struct{ DB *storage.DB }

func configHash(c Config) string {
	b, _ := json.Marshal(c)
	h := sha256.New()
	h.Write(b)
	// Bind the injected identity too. Rotating a source (including changing
	// the cluster in a kubeconfig across restart) invalidates pending grants.
	// Only the digest is persisted; no credential values enter the record.
	sources := map[string]string{}
	for _, p := range c.Profiles {
		for _, env := range []map[string]string{p.Env, p.WriteEnv} {
			for _, source := range env {
				sources[source] = source
			}
		}
		for _, source := range []string{p.KubeconfigEnv, p.WriteKubeconfigEnv} {
			if source != "" {
				sources[source] = source
			}
		}
	}
	for _, source := range sortedKeys(sources) {
		h.Write([]byte{0})
		h.Write([]byte(source))
		h.Write([]byte{0})
		value, exists := os.LookupEnv(source)
		if exists {
			h.Write([]byte{1})
			h.Write([]byte(value))
		}
	}
	for _, agent := range sortedKeys(c.agentVars) {
		for _, source := range sortedKeys(c.agentVars[agent]) {
			h.Write([]byte{0})
			h.Write([]byte("agent:" + agent + ":" + source))
			h.Write([]byte{0})
			h.Write([]byte(c.agentVars[agent][source]))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s Approvals) Create(ctx context.Context, c Config, agent, session, invocation, call string, req Request) (Approval, error) {
	r := Runtime{Config: c}
	ev := r.Check(agent, req)
	if ev.Decision != Prompt || !r.ApprovalEnabled(agent, req.Profile) || session == "" || call == "" {
		return Approval{}, ErrApproval
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Approval{}, err
	}
	o, _ := ctx.Value(originKey{}).(Origin)
	// CLI/scheduler requests can still be reviewed later in the dashboard.
	if o.Agent == "" && agent != "root" {
		o.Agent = agent
	}
	now := time.Now()
	a := Approval{ID: "approval_" + hex.EncodeToString(id[:]), SessionID: session, Agent: agent,
		InvocationID: invocation, CallID: call, Request: req, ConfigHash: configHash(c), Origin: o,
		Reason: ev.Reason, State: "pending", CreatedMS: now.UnixMilli(), ExpiresMS: now.Add(ApprovalTTL).UnixMilli()}
	b, _ := json.Marshal(req)
	origin, _ := json.Marshal(o)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO execution_approvals
	 (id,session_id,agent,invocation_id,call_id,request_json,config_hash,origin_json,reason,state,created_ms,expires_ms)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, a.ID, session, agent, invocation, call, string(b), a.ConfigHash, string(origin), a.Reason, a.State, a.CreatedMS, a.ExpiresMS)
	return a, err
}

const approvalColumns = `id,session_id,agent,invocation_id,call_id,request_json,config_hash,origin_json,reason,state,created_ms,expires_ms,resolved_ms,resolved_by,exec_id,outcome`

type scanner interface{ Scan(...any) error }

func scanApproval(row scanner) (Approval, error) {
	var a Approval
	var req, origin string
	err := row.Scan(&a.ID, &a.SessionID, &a.Agent, &a.InvocationID, &a.CallID, &req, &a.ConfigHash, &origin, &a.Reason, &a.State, &a.CreatedMS, &a.ExpiresMS, &a.ResolvedMS, &a.ResolvedBy, &a.ExecID, &a.Outcome)
	if err != nil {
		return a, err
	}
	if err = json.Unmarshal([]byte(req), &a.Request); err != nil {
		return a, err
	}
	err = json.Unmarshal([]byte(origin), &a.Origin)
	return a, err
}
func (s Approvals) Get(ctx context.Context, id string) (Approval, error) {
	a, err := scanApproval(s.DB.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM execution_approvals WHERE id=?`, id))
	if err != nil {
		return Approval{}, ErrApproval
	}
	return a, nil
}
func (s Approvals) List(ctx context.Context, session string, c Config) ([]Approval, error) {
	// Always include live requests, even if more than 100 later records exist.
	rows, err := s.DB.QueryContext(ctx, `SELECT `+approvalColumns+` FROM execution_approvals WHERE session_id=? AND
	 ((state='pending' AND expires_ms>?) OR id IN (SELECT id FROM execution_approvals WHERE session_id=? ORDER BY created_ms DESC LIMIT 100))
	 ORDER BY created_ms DESC,resolved_ms DESC,id DESC`, session, time.Now().UnixMilli(), session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		a.effectiveState(c)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (a *Approval) effectiveState(c Config) {
	if a.State == "pending" {
		if a.ExpiresMS <= time.Now().UnixMilli() {
			a.State = "expired"
		} else if a.ConfigHash != configHash(c) {
			a.State = "invalidated"
		}
	}
}

// Task pages need states, not full command bodies. Read a page's sessions in
// one query so a PostgreSQL round trip is not added for every conversation.
func (s Approvals) StatesForSessions(ctx context.Context, sessions []string, c Config) (map[string][]Approval, error) {
	out := map[string][]Approval{}
	err := storage.ForEachChunk(sessions, func(ids []string) error {
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		rows, err := s.DB.QueryContext(ctx, `SELECT id,session_id,invocation_id,state,config_hash,expires_ms,outcome FROM execution_approvals
		 WHERE session_id IN (`+storage.Placeholders(len(ids))+`) ORDER BY created_ms DESC,resolved_ms DESC,id DESC`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a Approval
			if err := rows.Scan(&a.ID, &a.SessionID, &a.InvocationID, &a.State, &a.ConfigHash, &a.ExpiresMS, &a.Outcome); err != nil {
				return err
			}
			a.effectiveState(c)
			out[a.SessionID] = append(out[a.SessionID], a)
		}
		return rows.Err()
	})
	return out, err
}

// Resolve is a compare-and-swap: two tabs cannot both grant the same request.
// An approved row alone cannot execute; the current server context must carry it.
func (s Approvals) Resolve(ctx context.Context, id, session, actor string, approved bool, c Config) error {
	return guardConfig(ctx, c, func(current Config) error {
		return s.resolve(ctx, id, session, actor, approved, current)
	})
}

func (s Approvals) resolve(ctx context.Context, id, session, actor string, approved bool, c Config) error {
	state := "rejected"
	if approved {
		state = "approved"
	}
	q := `UPDATE execution_approvals SET state=?,resolved_ms=?,resolved_by=? WHERE id=? AND session_id=? AND state='pending'`
	args := []any{state, time.Now().UnixMilli(), actor, id, session}
	if approved {
		q += ` AND expires_ms>? AND config_hash=?`
		args = append(args, time.Now().UnixMilli(), configHash(c))
	}
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrApproval
	}
	return nil
}
func (s Approvals) consume(ctx context.Context, c Config, agent, session, call string, req Request) (string, error) {
	return s.consumeIntent(ctx, c, agent, session, call, req, nil)
}
func (s Approvals) consumeIntent(ctx context.Context, c Config, agent, session, call string, req Request, intent *RunRecord) (string, error) {
	authority, _ := ctx.Value(approvalKey{}).(approvalAuthority)
	id := authority.id
	if id == "" {
		return "", ErrApproval
	}
	a, err := s.Get(ctx, id)
	if err != nil || a.Agent != agent || a.SessionID != session || a.CallID != call || a.Request != req {
		return "", ErrApproval
	}
	err = guardConfig(ctx, c, func(current Config) error {
		if configHash(current) != configHash(c) {
			return ErrApproval
		}
		return s.DB.InTx(ctx, func(tx *storage.Tx) error {
			res, err := tx.ExecContext(ctx, `UPDATE execution_approvals SET state='consumed' WHERE id=? AND state='approved' AND config_hash=? AND expires_ms>?`, id, configHash(current), time.Now().UnixMilli())
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return ErrApproval
			}
			if intent != nil {
				if intent.ApprovalID != id || intent.Agent != agent || intent.SessionID != session || intent.Profile != req.Profile {
					return ErrApproval
				}
				if err = insertIntent(ctx, tx, *intent); err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx, `UPDATE execution_approvals SET exec_id=? WHERE id=?`, intent.ExecID, id)
			}
			return err
		})
	})
	if err != nil {
		return "", err
	}
	return id, nil
}
func (s Approvals) Finish(ctx context.Context, id string, out Observation) error {
	result := "failed"
	if observationOutcome(out) == "unknown" {
		result = "unknown"
	}
	if observationOutcome(out) == "succeeded" {
		result = "succeeded"
	}
	// A janitor's unknown terminal outcome cannot be overwritten by a late
	// worker that wakes after its lease expired. Filling an empty outcome is
	// idempotent, including after the journal already saved a known result.
	_, err := s.DB.ExecContext(ctx, `UPDATE execution_approvals SET exec_id=?,outcome=? WHERE id=? AND state='consumed' AND outcome='' AND (exec_id='' OR exec_id=?)`, out.ExecID, result, id, out.ExecID)
	return err
}

// A disconnected or failed resume cannot leave a grant available for later.
func (s Approvals) Abandon(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = s.DB.ExecContext(ctx, `UPDATE execution_approvals SET state='abandoned' WHERE id=? AND state='approved'`, id)
}
func (r Runtime) WritesEnabled(agent, profile string) bool {
	for _, p := range r.Config.ProfilesFor(agent) {
		if p.Name == profile {
			return p.WriteApproval
		}
	}
	return false
}

// ApprovalEnabled reports whether this profile can ever turn a Prompt
// decision into a human confirmation at all — either because it grants
// elevated write credentials (WriteApproval) or because it may run a call
// unconfined once approved (AllowUnconfinedWithApproval). Both share the same
// Prompt → RequestConfirmation → ExecuteApproved pipeline; only the reason a
// Prompt was issued differs.
func (r Runtime) ApprovalEnabled(agent, profile string) bool {
	for _, p := range r.Config.ProfilesFor(agent) {
		if p.Name == profile {
			return p.WriteApproval || p.AllowUnconfinedWithApproval
		}
	}
	return false
}
func approvalError(err error) string { return fmt.Sprintf("未执行：%s", err) }
