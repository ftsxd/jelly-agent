package codeproject

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// StartSync launches a pull in the background and returns immediately with the
// task id.
//
// Running the clone inside the HTTP request was the thing that made a real
// repository impossible to pull: the request context died with the browser tab,
// the proxy, or the read timeout, and a 400 MB monorepo outlives all three. The
// goroutine takes a context detached from the request for exactly that reason.
//
// A project already syncing returns the existing task id rather than an error,
// so pressing the button twice never starts a second clone.
func (s *Store) StartSync(id string) (string, error) {
	p, taskID, limits, err := s.begin(id)
	if err != nil {
		return taskID, err
	}
	if taskID != "" && p.ID == "" {
		return taskID, nil // already running; caller gets the same task
	}
	go func() {
		defer s.release(id)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), limits.SyncTimeout)
		defer cancel()
		_ = s.perform(ctx, p, taskID, limits)
	}()
	return taskID, nil
}

// StartSyncFor is StartSync for an agent, which may only pull a project
// assigned to it.
//
// The grant is the only thing checked here. Whether the person actually agreed
// is not something this layer can know — the tool that calls it says so in its
// description, and the conversation is the record. What this does guarantee is
// that an agent cannot pull a repository it was never given.
func (s *Store) StartSyncFor(agent, id string) (string, error) {
	s.mu.RLock()
	ps, err := s.read()
	s.mu.RUnlock()
	if err != nil {
		return "", err
	}
	for _, p := range ps {
		if p.ID == id && allowed(p, agent) {
			return s.StartSync(id)
		}
	}
	return "", fmt.Errorf("%w（当前 Agent：%s）", ErrDenied, agent)
}

// SyncStateFor reports where a pull has got to, for an agent waiting on one.
// Grant-checked like every other agent-facing read.
func (s *Store) SyncStateFor(agent, id string) (state, revision, lastErr string, err error) {
	s.mu.RLock()
	ps, rerr := s.read()
	s.mu.RUnlock()
	if rerr != nil {
		return "", "", "", rerr
	}
	for _, p := range ps {
		if p.ID == id && allowed(p, agent) {
			return p.SyncState, p.Revision, p.LastError, nil
		}
	}
	return "", "", "", fmt.Errorf("%w（当前 Agent：%s）", ErrDenied, agent)
}

// Sync pulls synchronously. The HTTP path uses StartSync; this stays for tests
// and for any caller that genuinely wants to wait.
func (s *Store) Sync(ctx context.Context, id string) error {
	p, taskID, limits, err := s.begin(id)
	if err != nil {
		return err
	}
	if p.ID == "" {
		return ErrBusy
	}
	defer s.release(id)
	ctx, cancel := context.WithTimeout(ctx, limits.SyncTimeout)
	defer cancel()
	return s.perform(ctx, p, taskID, limits)
}

// begin reserves the project and records the queued state on disk. A zero
// Project with a non-empty task id means a sync was already in flight.
func (s *Store) begin(id string) (Project, string, Limits, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.busy[id]; t != "" {
		return Project{}, t, s.limits, nil
	}
	ps, err := s.read()
	if err != nil {
		return Project{}, "", s.limits, err
	}
	for i := range ps {
		if ps[i].ID != id {
			continue
		}
		taskID := newTaskID()
		now := time.Now().UTC()
		ps[i].SyncState = SyncQueued
		ps[i].SyncTaskID = taskID
		ps[i].SyncStartedAt = &now
		ps[i].LastError = ""
		if err := s.write(ps); err != nil {
			return Project{}, "", s.limits, err
		}
		s.busy[id] = taskID
		return ps[i], taskID, s.limits, nil
	}
	return Project{}, "", s.limits, ErrNotFound
}

func (s *Store) release(id string) {
	s.mu.Lock()
	delete(s.busy, id)
	s.mu.Unlock()
}

// perform clones and publishes. It runs outside the metadata lock so assigning
// an agent stays available meanwhile, and only a complete snapshot is ever
// published. No repository scripts are executed.
func (s *Store) perform(ctx context.Context, p Project, taskID string, limits Limits) error {
	s.setState(p.ID, taskID, SyncRunning)
	snapshot, revision, syncErr := s.clone(ctx, p, limits)
	var issues []string
	if syncErr == nil {
		issues = checkDirs(filepath.Join(s.dir, snapshot), p)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Clear the reservation in the same locked section that records the
	// outcome. Doing it in a separate deferred call left a window where the
	// state already said "failed" while the project still reported as syncing.
	// StartSync/Sync release again on the way out, which is a no-op by then.
	delete(s.busy, p.ID)
	ps, err := s.read()
	if err != nil {
		s.removeSnapshot(snapshot)
		return err
	}
	for i := range ps {
		if ps[i].ID != p.ID {
			continue
		}
		oldSnapshot := ps[i].Snapshot
		ps[i].SyncTaskID = ""
		if syncErr != nil {
			ps[i].SyncState = SyncFailed
			ps[i].LastError = syncErr.Error()
		} else {
			now := time.Now().UTC()
			ps[i].SyncState = SyncSucceeded
			ps[i].Snapshot = snapshot
			ps[i].Revision = revision
			ps[i].SyncedAt = &now
			ps[i].LastError = ""
			ps[i].DirIssues = issues
			// Whatever an agent was waiting for, this is it. Leaving the card
			// up after the pull would ask the operator to approve a sync that
			// already happened.
			ps[i].SyncRequest = nil
		}
		if err = s.write(ps); err != nil {
			s.removeSnapshot(snapshot)
			return err
		}
		if syncErr == nil && oldSnapshot != snapshot {
			s.removeSnapshot(oldSnapshot)
		}
		return syncErr
	}
	s.removeSnapshot(snapshot)
	return ErrNotFound
}

// setState records a transition without holding the lock across the clone.
func (s *Store) setState(id, taskID, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := s.read()
	if err != nil {
		return
	}
	for i := range ps {
		if ps[i].ID == id && ps[i].SyncTaskID == taskID {
			ps[i].SyncState = state
			_ = s.write(ps)
			return
		}
	}
}

// checkDirs reports configured directories that are missing from the snapshot
// or are not directories. A repository reorganisation should say which path to
// fix, not silently return empty search results.
func checkDirs(snapshot string, p Project) []string {
	var issues []string
	for _, d := range p.Directories() {
		if d.Path == "." {
			continue
		}
		info, err := os.Stat(filepath.Join(snapshot, filepath.FromSlash(d.Path)))
		if err != nil {
			issues = append(issues, d.Path+"（不存在）")
		} else if !info.IsDir() {
			issues = append(issues, d.Path+"（不是目录）")
		}
	}
	return issues
}

func newTaskID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("sync-%d", time.Now().UnixNano())
	}
	return "sync-" + hex.EncodeToString(b)
}

// gitSession is one authenticated git environment: an isolated HOME, the
// credential wired in as a header, and the material needed to scrub that
// credential back out of anything git says. Shared by the clone and the much
// cheaper remote-revision check so the two can never disagree about how this
// repository authenticates.
type gitSession struct {
	env     []string
	token   string
	auth    string
	work    string
	stderr  boundedBuffer
	cleanup func()
}

func (s *Store) newGitSession(p Project) (*gitSession, error) {
	if err := Validate(p); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(s.dir, ".pull-")
	if err != nil {
		return nil, err
	}
	g := &gitSession{work: work, cleanup: func() { os.RemoveAll(work) }}
	g.stderr.max = 8 << 10
	g.env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + work, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1"}
	// A token saved from the console wins over the environment variable. In a
	// container the variable is the weaker option anyway: docker inspect and
	// /proc/1/environ show it to anyone who can reach the host or exec in.
	g.token = s.tokenFor(p.ID)
	if g.token == "" && p.TokenEnv != "" {
		if g.token = os.Getenv(p.TokenEnv); g.token == "" {
			g.cleanup()
			return nil, fmt.Errorf("凭据环境变量未设置：%s（或改为在页面上直接填写访问令牌）", p.TokenEnv)
		}
	}
	if g.token != "" {
		username := p.Username
		if username == "" {
			username = "oauth2"
		}
		g.auth = base64.StdEncoding.EncodeToString([]byte(username + ":" + g.token))
		g.env = append(g.env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http."+p.URL+".extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+g.auth)
	}
	return g, nil
}

// run executes git with the hardening flags. Stderr is captured rather than
// discarded: discarding it meant every failure — a 401, a typo in the URL, a
// missing branch, no DNS — arrived as the same sentence listing five things to
// check, which is not a diagnosis. It is scrubbed of the credential before it
// goes anywhere, because last_error is persisted into projects.json.
func (g *gitSession) run(ctx context.Context, args ...string) (string, error) {
	args = append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "credential.helper=", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "http.followRedirects=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = g.env
	cmd.WaitDelay = 2 * time.Second
	g.stderr.b.Reset()
	cmd.Stderr = &g.stderr
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
}

func (s *Store) clone(ctx context.Context, p Project, limits Limits) (string, string, error) {
	g, err := s.newGitSession(p)
	if err != nil {
		return "", "", err
	}
	defer g.cleanup()
	work, token, auth := g.work, g.token, g.auth
	stderr := &g.stderr
	run := func(args ...string) (string, error) { return g.run(ctx, args...) }
	checkout := filepath.Join(work, "checkout")
	if _, err := run("clone", "--depth=1", "--single-branch", "--no-tags", "--branch", p.Branch, "--", p.URL, checkout); err != nil {
		if ctx.Err() != nil {
			return "", "", fmt.Errorf("拉取已取消或超过 %s，请重试或调高 files.code_projects.sync_timeout_sec", limits.SyncTimeout)
		}
		return "", "", errors.New(diagnose(stderr.b.String(), token, auth, p, err))
	}
	revision, err := run("-C", checkout, "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("无法读取代码版本")
	}
	if err := os.RemoveAll(filepath.Join(checkout, ".git")); err != nil {
		return "", "", err
	}
	// Exclude links and special files; a snapshot only contains regular code.
	var total int64
	var count int
	err = filepath.WalkDir(checkout, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return os.Remove(path)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		count++
		if total > limits.MaxSnapshotBytes {
			return fmt.Errorf("代码快照超过 %d MB 上限，请调高 files.code_projects.max_snapshot_mb，或改用只含所需目录的项目", limits.MaxSnapshotBytes>>20)
		}
		if count > limits.MaxSnapshotFiles {
			return fmt.Errorf("代码快照超过 %d 个文件上限，请调高 files.code_projects.max_snapshot_files", limits.MaxSnapshotFiles)
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	dest, err := os.MkdirTemp(s.dir, "snapshot-")
	if err != nil {
		return "", "", err
	}
	// Rename into a reserved name so no partially copied tree is ever visible.
	if err = os.Remove(dest); err != nil {
		return "", "", err
	}
	if err = os.Rename(checkout, dest); err != nil {
		return "", "", err
	}
	return filepath.Base(dest), revision, nil
}

// RemoteStatus is the answer to "is the snapshot I would analyse still current".
type RemoteStatus struct {
	Local  string `json:"local_revision,omitempty"`
	Remote string `json:"remote_revision"`
	Behind bool   `json:"behind"`
}

// Probe ages. A snapshot that just went stale is worth knowing about within
// minutes, not seconds; a remote that cannot be reached is worth retrying
// sooner than that, but not on every turn.
const (
	RemoteProbeTTL    = 5 * time.Minute
	remoteProbeErrTTL = 30 * time.Second
)

// CheckRemote asks the server for the branch head without downloading anything.
// One ls-remote is about a second against a real monorepo, where the clone it
// would otherwise take to find out is 352 MB — so "am I current" is a question
// worth answering separately from "make me current".
//
// Always goes to the remote: this is the console's 检查更新 button, and a
// person who clicks it is asking for the current answer.
func (s *Store) CheckRemote(ctx context.Context, id string) (RemoteStatus, error) {
	return s.CheckRemoteCached(ctx, id, 0)
}

// CheckRemoteCached answers from the last probe when it is younger than maxAge,
// and otherwise asks the remote and remembers the answer. maxAge of 0 always
// asks.
//
// Failures are remembered too, for a shorter while: an unreachable git server
// or an expired token fails the same way on every retry, and the agent path
// calls this at the start of every analysis.
func (s *Store) CheckRemoteCached(ctx context.Context, id string, maxAge time.Duration) (RemoteStatus, error) {
	if maxAge > 0 {
		s.probeMu.Lock()
		e, ok := s.probes[id]
		s.probeMu.Unlock()
		if ok {
			ttl := maxAge
			if e.err != nil {
				ttl = min(maxAge, remoteProbeErrTTL)
			}
			if time.Since(e.at) < ttl {
				return e.st, e.err
			}
		}
	}
	st, err := s.checkRemote(ctx, id)
	s.probeMu.Lock()
	if s.probes == nil {
		s.probes = map[string]remoteProbe{}
	}
	s.probes[id] = remoteProbe{st: st, err: err, at: time.Now()}
	s.probeMu.Unlock()
	return st, err
}

// CheckRemoteFor is CheckRemoteCached for an agent, which may only ask about a
// project assigned to it. The grant is checked here for the same reason the
// read tools check it on every call: an assignment can be withdrawn while a
// conversation is still going.
func (s *Store) CheckRemoteFor(ctx context.Context, agent, id string, maxAge time.Duration) (RemoteStatus, error) {
	s.probeMu.Lock()
	on := s.autoProbe
	s.probeMu.Unlock()
	if !on {
		return RemoteStatus{}, ErrProbeOff
	}
	s.mu.RLock()
	ps, err := s.read()
	s.mu.RUnlock()
	if err != nil {
		return RemoteStatus{}, err
	}
	for _, p := range ps {
		if p.ID == id && allowed(p, agent) {
			return s.CheckRemoteCached(ctx, id, maxAge)
		}
	}
	return RemoteStatus{}, fmt.Errorf("%w（当前 Agent：%s）", ErrDenied, agent)
}

// SeedRemoteProbeForTest plants a freshness answer, so a test can exercise the
// "snapshot is behind" path without standing up a git server — the repository
// URL is required to be HTTPS, which is what stops a local fixture repo from
// standing in for one. Nothing in production calls it.
func (s *Store) SeedRemoteProbeForTest(id string, st RemoteStatus) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if s.probes == nil {
		s.probes = map[string]remoteProbe{}
	}
	s.probes[id] = remoteProbe{st: st, at: time.Now()}
}

func (s *Store) checkRemote(ctx context.Context, id string) (RemoteStatus, error) {
	s.mu.RLock()
	var p *Project
	ps, err := s.read()
	if err == nil {
		for i := range ps {
			if ps[i].ID == id {
				cp := ps[i]
				p = &cp
				break
			}
		}
	}
	s.mu.RUnlock()
	if err != nil {
		return RemoteStatus{}, err
	}
	if p == nil {
		return RemoteStatus{}, ErrNotFound
	}

	g, err := s.newGitSession(*p)
	if err != nil {
		return RemoteStatus{}, err
	}
	defer g.cleanup()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	out, err := g.run(ctx, "ls-remote", "--heads", "--", p.URL, p.Branch)
	if err != nil {
		if ctx.Err() != nil {
			return RemoteStatus{}, errors.New("检查更新超时，请稍后重试")
		}
		return RemoteStatus{}, errors.New(diagnose(g.stderr.b.String(), g.token, g.auth, *p, err))
	}
	// "<sha>\trefs/heads/<branch>" — an empty result means the branch is gone.
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return RemoteStatus{}, fmt.Errorf("远端没有分支 %s：请确认分支名", p.Branch)
	}
	remote := fields[0]
	return RemoteStatus{Local: p.Revision, Remote: remote, Behind: p.Revision != "" && p.Revision != remote}, nil
}
