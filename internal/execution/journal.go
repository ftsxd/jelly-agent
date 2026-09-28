package execution

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// Never store argv, environment, credentials or output here. The record is
// resource authority, retained independently of the chat/session lifetime.
const journalSchema = `CREATE TABLE IF NOT EXISTS execution_runs (
 exec_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, agent TEXT NOT NULL,
 profile TEXT NOT NULL, approval_id TEXT NOT NULL, workspace_root TEXT NOT NULL,
 scope TEXT NOT NULL, owner TEXT NOT NULL, daemon TEXT NOT NULL,
 container_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
 created_ms BIGINT NOT NULL, deadline_ms BIGINT NOT NULL, ended_ms BIGINT NOT NULL DEFAULT 0,
 cleanup_state TEXT NOT NULL DEFAULT 'pending', cleanup_owner TEXT NOT NULL DEFAULT '',
 cleanup_until_ms BIGINT NOT NULL DEFAULT 0, cleanup_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS execution_runs_recovery ON execution_runs(scope, cleanup_state, deadline_ms);`

func EnsureJournalSchema(db *storage.DB) error {
	return storage.ApplySchema(db, journalSchema, "execution_runs")
}

type Journal struct {
	DB   *storage.DB
	Root string
}
type RunRecord struct {
	ExecID, SessionID, Agent, Profile, ApprovalID, Root, Scope, Owner, Daemon, ContainerID, State string
	CreatedMS, DeadlineMS, EndedMS                                                                int64
	CleanupState, CleanupOwner                                                                    string
	CleanupUntilMS                                                                                int64
	CleanupError                                                                                  string
}

const runColumns = `exec_id,session_id,agent,profile,approval_id,workspace_root,scope,owner,daemon,container_id,state,created_ms,deadline_ms,ended_ms,cleanup_state,cleanup_owner,cleanup_until_ms,cleanup_error`

var runID = regexp.MustCompile(`^exec_[a-f0-9]{32}$`)
var nonceID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func nonce() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func scanRun(s scanner) (r RunRecord, err error) {
	err = s.Scan(&r.ExecID, &r.SessionID, &r.Agent, &r.Profile, &r.ApprovalID, &r.Root, &r.Scope, &r.Owner, &r.Daemon, &r.ContainerID, &r.State, &r.CreatedMS, &r.DeadlineMS, &r.EndedMS, &r.CleanupState, &r.CleanupOwner, &r.CleanupUntilMS, &r.CleanupError)
	return
}
func (j Journal) Get(ctx context.Context, id string) (RunRecord, error) {
	return scanRun(j.DB.QueryRowContext(ctx, "SELECT "+runColumns+" FROM execution_runs WHERE exec_id=?", id))
}

// The root and its scope marker live outside the child's writable mount. A
// different host/root cannot claim these rows merely because it shares a DB.
func (j Journal) openRoot(create bool) (*os.Root, string, error) {
	if !filepath.IsAbs(j.Root) {
		return nil, "", fmt.Errorf("执行资源目录必须为绝对路径")
	}
	if create {
		if err := os.MkdirAll(j.Root, 0700); err != nil {
			return nil, "", err
		}
	}
	st, err := os.Lstat(j.Root)
	if err != nil {
		return nil, "", err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
		return nil, "", fmt.Errorf("执行资源目录必须为私有目录且不能是符号链接")
	}
	root, err := os.OpenRoot(j.Root)
	if err != nil {
		return nil, "", err
	}
	fail := func(err error) (*os.Root, string, error) { root.Close(); return nil, "", err }
	st, err = root.Lstat(".scope")
	if errors.Is(err, os.ErrNotExist) && create {
		// Publish a complete, synced file atomically. Concurrent initialisers
		// use an exclusive hard link; no caller observes a partially written nonce.
		scope, e := nonce()
		if e != nil {
			return fail(e)
		}
		name := ".scope-" + scope
		f, e := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return fail(e)
		}
		_, e = f.WriteString(scope)
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e == nil {
			e = closeErr
		}
		if e == nil {
			e = root.Link(name, ".scope")
		}
		root.Remove(name)
		if e != nil && !errors.Is(e, os.ErrExist) {
			return fail(e)
		}
		st, err = root.Lstat(".scope")
	}
	if err != nil {
		return fail(err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return fail(fmt.Errorf("执行资源范围标记无效"))
	}
	b, err := root.ReadFile(".scope")
	if err != nil {
		return fail(err)
	}
	scope := string(b)
	if !nonceID.MatchString(scope) {
		return fail(fmt.Errorf("执行资源范围标记无效"))
	}
	return root, scope, nil
}

// Intent is committed before the workspace or any container can exist. The
// prepared lease includes startup grace; start CAS fences an expired cleaner.
func (j Journal) Prepare(id, agent, session, profile, approval, daemon string, timeout time.Duration) (RunRecord, error) {
	if !runID.MatchString(id) || daemon == "" {
		return RunRecord{}, fmt.Errorf("执行资源身份无效")
	}
	root, scope, err := j.openRoot(true)
	if err != nil {
		return RunRecord{}, err
	}
	defer root.Close()
	owner, err := nonce()
	if err != nil {
		return RunRecord{}, err
	}
	now := time.Now()
	r := RunRecord{ExecID: id, Agent: agent, SessionID: session, Profile: profile, ApprovalID: approval, Root: j.Root, Scope: scope, Owner: owner, Daemon: daemon, State: "prepared", CleanupState: "pending", CreatedMS: now.UnixMilli(), DeadlineMS: now.Add(timeout + 30*time.Second).UnixMilli()}
	return r, nil
}

type journalWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertIntent(ctx context.Context, db journalWriter, r RunRecord) error {
	_, err := db.ExecContext(ctx, `INSERT INTO execution_runs(exec_id,session_id,agent,profile,approval_id,workspace_root,scope,owner,daemon,state,created_ms,deadline_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, r.ExecID, r.SessionID, r.Agent, r.Profile, r.ApprovalID, r.Root, r.Scope, r.Owner, r.Daemon, r.State, r.CreatedMS, r.DeadlineMS)
	return err
}
func (j Journal) Workspace(ctx context.Context, r RunRecord) (string, error) {
	root, scope, err := j.openRoot(false)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if r.Root != j.Root || r.Scope != scope || !runID.MatchString(r.ExecID) {
		return "", fmt.Errorf("执行目录范围不匹配")
	}
	current, err := j.Get(ctx, r.ExecID)
	if err != nil {
		return "", err
	}
	if current.Owner != r.Owner || current.State != "prepared" || current.CleanupState != "pending" || current.DeadlineMS <= time.Now().UnixMilli() {
		return "", fmt.Errorf("执行准备租约已失效")
	}
	if err = root.Mkdir(r.ExecID, 0700); err != nil {
		return "", err
	}
	return filepath.Join(j.Root, r.ExecID), nil
}
func (j Journal) Begin(ctx context.Context, id, agent, session, profile, approval, daemon string, timeout time.Duration) (RunRecord, string, error) {
	r, err := j.Prepare(id, agent, session, profile, approval, daemon, timeout)
	if err != nil {
		return RunRecord{}, "", err
	}
	err = insertIntent(ctx, j.DB, r)
	if err != nil {
		return RunRecord{}, "", err
	}
	dir, err := j.Workspace(ctx, r)
	return r, dir, err
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("执行租约已失效或资源回收已开始，禁止启动/重放")
	}
	return nil
}
func (j Journal) Created(ctx context.Context, r RunRecord, cid string) error {
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(cid) {
		return fmt.Errorf("容器身份无效")
	}
	return affected(j.DB.ExecContext(ctx, `UPDATE execution_runs SET container_id=? WHERE exec_id=? AND owner=? AND state='prepared' AND container_id='' AND cleanup_state='pending' AND deadline_ms>?`, cid, r.ExecID, r.Owner, time.Now().UnixMilli()))
}
func (j Journal) Starting(ctx context.Context, r RunRecord, timeout time.Duration) error {
	now := time.Now()
	return affected(j.DB.ExecContext(ctx, `UPDATE execution_runs SET state='running',deadline_ms=? WHERE exec_id=? AND owner=? AND state='prepared' AND container_id<>'' AND cleanup_state='pending' AND deadline_ms>?`, now.Add(timeout+30*time.Second).UnixMilli(), r.ExecID, r.Owner, now.UnixMilli()))
}
func (j Journal) Finish(ctx context.Context, r RunRecord, out Observation) error {
	state := observationOutcome(out)
	return affected(j.DB.ExecContext(ctx, `UPDATE execution_runs SET state=?,ended_ms=? WHERE exec_id=? AND owner=? AND state IN ('prepared','running') AND cleanup_state='pending'`, state, time.Now().UnixMilli(), r.ExecID, r.Owner))
}
func identity(r RunRecord) sandbox.ContainerIdentity {
	return sandbox.ContainerIdentity{Name: "jelly-" + r.ExecID, ID: r.ContainerID, ExecID: r.ExecID, Owner: r.Owner, Scope: r.Scope, Daemon: r.Daemon}
}

// Cleanup never executes a business command. An expired in-flight record is
// made unknown before touching resources; consumed approval remains consumed.
func (j Journal) Cleanup(ctx context.Context, id string) error {
	root, scope, err := j.openRoot(false)
	if err != nil {
		return err
	}
	defer root.Close()
	r, err := j.Get(ctx, id)
	if err != nil {
		return err
	}
	if r.Root != j.Root || r.Scope != scope || !runID.MatchString(r.ExecID) || !nonceID.MatchString(r.Owner) {
		return fmt.Errorf("执行资源不属于当前恢复范围")
	}
	now := time.Now().UnixMilli()
	claim, err := nonce()
	if err != nil {
		return err
	}
	res, err := j.DB.ExecContext(ctx, `UPDATE execution_runs SET cleanup_state='cleaning',cleanup_owner=?,cleanup_until_ms=?,state=CASE WHEN state IN ('prepared','running') THEN 'unknown' ELSE state END,ended_ms=CASE WHEN ended_ms=0 THEN ? ELSE ended_ms END WHERE exec_id=? AND owner=? AND (state NOT IN ('prepared','running') OR deadline_ms<=?) AND (cleanup_state IN ('pending','cleaned') OR (cleanup_state='cleaning' AND cleanup_until_ms<=?))`, claim, now+30000, now, id, r.Owner, now, now)
	if err = affected(res, err); err != nil {
		return err
	}
	// Finish may have committed between the initial read and the claim. Read
	// the claimed state/ID rather than turning its known success into unknown.
	r, err = j.Get(ctx, id)
	if err != nil {
		return err
	}
	if r.CleanupOwner != claim {
		return fmt.Errorf("执行回收租约已改变")
	}
	// Complete approval evidence before resource removal. If storage fails,
	// leave the claim recoverable; no retry of the original request is allowed.
	if r.ApprovalID != "" {
		outcome := "unknown"
		if r.State == "succeeded" {
			outcome = "succeeded"
		} else if r.State == "failed" || r.State == "timed_out" || r.State == "cancelled" {
			outcome = "failed"
		}
		_, err = j.DB.ExecContext(ctx, `UPDATE execution_approvals SET exec_id=?,outcome=? WHERE id=? AND state='consumed' AND outcome=''`, id, outcome, r.ApprovalID)
		if err != nil {
			return err
		}
	}
	container, absent, cleanupErr := sandbox.InspectManaged(ctx, identity(r))
	if cleanupErr == nil && !absent {
		if r.ContainerID == "" {
			cleanupErr = affected(j.DB.ExecContext(ctx, `UPDATE execution_runs SET container_id=? WHERE exec_id=? AND owner=? AND container_id='' AND cleanup_owner=?`, container.ID, id, r.Owner, claim))
		}
		if cleanupErr == nil {
			r.ContainerID = container.ID
			cleanupErr = sandbox.RemoveManaged(ctx, identity(r))
		}
	}
	if cleanupErr == nil {
		st, e := root.Lstat(id)
		switch {
		case errors.Is(e, os.ErrNotExist):
		case e != nil:
			cleanupErr = e
		case !st.IsDir() || st.Mode()&os.ModeSymlink != 0:
			cleanupErr = fmt.Errorf("执行目录身份异常，拒绝清理")
		default:
			if cleanupErr = root.Chmod(id, 0700); cleanupErr != nil {
				break
			}
			child, e := root.OpenRoot(id)
			if e != nil {
				cleanupErr = e
			} else {
				cleanupErr = root.RemoveAll(id)
				if cleanupErr != nil {
					if e = restoreWorkspaceDirs(child, "."); e == nil {
						cleanupErr = root.RemoveAll(id)
					} else {
						cleanupErr = e
					}
				}
				child.Close()
			}
		}
	}
	state, msg := "cleaned", ""
	if cleanupErr != nil {
		state = "pending"
		msg = "资源回收未确认；保留恢复记录及凭据目录，禁止重放"
	}
	_, storeErr := j.DB.ExecContext(ctx, `UPDATE execution_runs SET cleanup_state=?,cleanup_error=?,cleanup_until_ms=0 WHERE exec_id=? AND cleanup_owner=?`, state, msg, id, claim)
	if cleanupErr != nil {
		return fmt.Errorf("%s：%w", msg, cleanupErr)
	}
	return storeErr
}

// Recover also scans retained tombstones: a create RPC may complete after a
// first not-found cleanup. Labels require the original row and nonce; unknown
// or foreign resources are never removed. Rows must not cascade with sessions.
func (j Journal) Recover(ctx context.Context) (int, error) {
	root, scope, err := j.openRoot(false)
	if errors.Is(err, os.ErrNotExist) {
		var n int
		if e := j.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_runs WHERE workspace_root=?`, j.Root).Scan(&n); e != nil {
			return 0, e
		}
		if n == 0 {
			return 0, nil
		}
		return 0, fmt.Errorf("执行资源范围标记不可用，保留恢复记录")
	}
	if err != nil {
		return 0, err
	}
	defer root.Close()
	rows, err := j.DB.QueryContext(ctx, `SELECT `+runColumns+` FROM execution_runs WHERE workspace_root=? AND scope=?`, j.Root, scope)
	if err != nil {
		return 0, err
	}
	all := map[string]RunRecord{}
	daemons := map[string]bool{}
	var candidates []string
	now := time.Now().UnixMilli()
	for rows.Next() {
		r, e := scanRun(rows)
		if e != nil {
			rows.Close()
			return 0, e
		}
		all[r.ExecID] = r
		daemons[r.Daemon] = true
		if r.CleanupState != "cleaned" && (r.State != "prepared" && r.State != "running" || r.DeadlineMS <= now) && (r.CleanupState != "cleaning" || r.CleanupUntilMS <= now) {
			candidates = append(candidates, r.ExecID)
		}
		if r.CleanupState == "cleaned" && runID.MatchString(r.ExecID) {
			if _, e := root.Lstat(r.ExecID); !errors.Is(e, os.ErrNotExist) {
				candidates = append(candidates, r.ExecID)
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	var problems []error
	for daemon := range daemons {
		found, e := sandbox.DiscoverManaged(ctx, scope, daemon)
		if e != nil {
			problems = append(problems, e)
			continue
		}
		for _, info := range found {
			r, ok := all[info.Labels[sandbox.LabelExec]]
			if !ok || r.Owner != info.Labels[sandbox.LabelOwner] || r.Daemon != daemon {
				problems = append(problems, fmt.Errorf("发现无匹配执行记录的容器，已保留"))
				continue
			}
			if (r.State == "prepared" || r.State == "running") && r.DeadlineMS > now {
				continue
			}
			check := identity(r)
			check.ID = info.ID
			if r.ContainerID != "" && r.ContainerID != info.ID {
				problems = append(problems, fmt.Errorf("遗留容器 ID 与执行记录不匹配"))
				continue
			}
			if _, _, e = sandbox.InspectManaged(ctx, check); e != nil {
				problems = append(problems, e)
				continue
			}
			if r.ContainerID == "" {
				if e = affected(j.DB.ExecContext(ctx, `UPDATE execution_runs SET container_id=? WHERE exec_id=? AND owner=? AND container_id='' AND (state NOT IN ('prepared','running') OR deadline_ms<=?)`, info.ID, r.ExecID, r.Owner, now)); e != nil {
					problems = append(problems, e)
					continue
				}
			}
			candidates = append(candidates, r.ExecID)
		}
	}
	n := 0
	seen := map[string]bool{}
	for _, id := range candidates {
		if seen[id] {
			continue
		}
		seen[id] = true
		if e := j.Cleanup(ctx, id); e != nil {
			problems = append(problems, e)
		} else {
			n++
		}
		if ctx.Err() != nil {
			break
		}
	}
	return n, errors.Join(problems...)
}

// RootForState binds the local resource journal to the configured SQLite
// directory; PostgreSQL deployments keep their host scope beside the config.
func RootForState(ref, configDir string) (string, error) {
	dir := filepath.Dir(ref)
	if strings.Contains(ref, "://") {
		dir = configDir
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			dir = filepath.Join(home, ".jelly-agent")
		}
	}
	return filepath.Abs(filepath.Join(dir, "execution-workspaces"))
}
