package codeproject

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Read-only git history over the project's own object cache.
//
// The published snapshot has no .git and is not going to get one: it is an
// extracted archive precisely so that nothing under it can be a repository an
// agent walks into. But "这个目录最近改了什么" is the second question anyone
// asks about a service, and answering it with 我的工具只能读文件 is a hole in
// code analysis, not a safety property.
//
// So history is read from the bare cache the incremental sync already keeps
// beside the snapshot — the same objects that were fetched to build it. That
// gives log/show/diff with no extra download, no credential in play (nothing
// here touches the network), and no repository for the model to escape into:
// every command below runs against a fixed directory with a validated revision
// and a pathspec pinned to the project's configured scope.
//
// What it cannot do is see past the fetch depth. A shallow cache is the whole
// reason the sync is cheap, so the honest answer to "只有 3 个提交" is "本地只
// 存了这么多", and every result says so rather than letting the model report a
// fetch depth as a repository's history.

// ErrNoHistory is the state a project is in before the first incremental sync:
// a snapshot exists, but the object cache that history is read from does not.
var ErrNoHistory = errors.New("这个项目还没有本地提交历史：请先同步一次代码（sync_project）再查历史")

// revisionArg is what a caller may name a commit with. Deliberately narrow:
// only the cache's own object ids, optionally with an ancestor suffix. Branch
// and tag names are not merely unsupported, they are meaningless here — the
// cache holds exactly one ref — and anything looser would be a string from a
// model reaching a command line.
var revisionArg = regexp.MustCompile(`^(?i:HEAD|[0-9a-f]{4,40})(\^|~\d{1,4})?$`)

// Bounds on one history answer. A patch is model context, not a file: past a
// certain size it stops being evidence and starts being the whole budget, and
// the fix is a narrower path, not a bigger ceiling.
const (
	// DefaultHistoryDepth is how many commits a sync keeps. Deep enough for
	// "最近有什么改动" on an active service, shallow enough that it costs a few
	// hundred KB of deltas on top of the tip tree rather than a second clone.
	DefaultHistoryDepth = 50
	maxHistoryDepth     = 2000
	defaultLogLimit     = 20
	maxLogLimit         = 100
	maxFilesListed      = 300
)

// historyDepth is how many commits this project's next fetch keeps: the
// project's own setting first, then the deployment's, then the default. The
// per-project value wins because the cost is a property of the repository, not
// of the server it is configured on.
func historyDepth(p Project, limits Limits) int {
	for _, d := range []int{p.HistoryDepth, limits.HistoryDepth, DefaultHistoryDepth} {
		if d > 0 {
			return min(d, maxHistoryDepth)
		}
	}
	return DefaultHistoryDepth
}

// maxPatchBytes is how much patch one answer may carry. A var so a test can
// drive the truncation path without generating a megabyte of diff: past this
// the useful move is a narrower path, not a bigger ceiling.
var maxPatchBytes = 60 << 10

// Commit is one entry of history as the model sees it.
type Commit struct {
	Revision string       `json:"revision"`
	Author   string       `json:"author,omitempty"`
	Date     string       `json:"date,omitempty"`
	Subject  string       `json:"subject"`
	Body     string       `json:"body,omitempty"`
	Files    []FileChange `json:"files,omitempty"`
}

// FileChange is one path touched by a commit or a diff. Added/Deleted are -1
// for binary files, which is what git reports as "-".
type FileChange struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Status  string `json:"status,omitempty"`
	Added   int    `json:"added,omitempty"`
	Deleted int    `json:"deleted,omitempty"`
	Binary  bool   `json:"binary,omitempty"`
}

// history is the part every result repeats: what the answer was actually read
// from. A commit list with no depth attached reads as the repository's history,
// and that is the one thing it is not.
type history struct {
	Revision  string   `json:"snapshot_revision"`
	Scope     []string `json:"scope,omitempty"`
	Depth     int      `json:"local_history_commits"`
	Shallow   bool     `json:"shallow,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Note      string   `json:"note,omitempty"`
}

// LogQuery is one "what changed" question.
type LogQuery struct {
	Path      string
	Limit     int
	Since     string
	Author    string
	Keyword   string
	WithFiles bool
}

type LogResult struct {
	history
	Commits []Commit `json:"commits"`
}

// ShowQuery is one commit, optionally with its patch.
type ShowQuery struct {
	Revision string
	Path     string
	Patch    bool
}

type ShowResult struct {
	history
	Commit Commit `json:"commit"`
	Patch  string `json:"patch,omitempty"`
}

// DiffQuery compares two revisions the caller already learned from a log.
type DiffQuery struct {
	From  string
	To    string
	Path  string
	Patch bool
}

type DiffResult struct {
	history
	From  string       `json:"from"`
	To    string       `json:"to"`
	Files []FileChange `json:"files"`
	Patch string       `json:"patch,omitempty"`
}

// LogFor lists commits behind the published snapshot, newest first.
func (s *Store) LogFor(ctx context.Context, agent, id string, q LogQuery) (LogResult, error) {
	var out LogResult
	err := s.withHistory(agent, id, q.Path, func(g *localGit, p Project, spec []string) error {
		limit := q.Limit
		if limit <= 0 {
			limit = defaultLogLimit
		}
		if limit > maxLogLimit {
			limit = maxLogLimit
		}
		head := historyHead(p)
		args := []string{"log", "--max-count=" + strconv.Itoa(limit), "--date=iso-strict", "--no-color", "--format=" + logFormat}
		if q.WithFiles {
			args = append(args, "--name-status", "--find-renames")
		}
		if v := strings.TrimSpace(q.Since); v != "" {
			args = append(args, "--since="+v)
		}
		if v := strings.TrimSpace(q.Author); v != "" {
			args = append(args, "--author="+v)
		}
		if v := strings.TrimSpace(q.Keyword); v != "" {
			args = append(args, "--grep="+v, "--regexp-ignore-case")
		}
		args = append(args, head)
		args = append(args, pathspecArgs(spec)...)

		raw, _, err := g.run(ctx, 1<<20, args...)
		if err != nil {
			return err
		}
		out.Commits = parseLog(raw)
		out.history = g.state(ctx, p, spec)
		if len(out.Commits) == 0 {
			out.Note = strings.TrimSpace("这个范围内没有提交。" + out.Note)
		}
		return nil
	})
	return out, err
}

// ShowFor reads one commit: who, when, which files, and optionally the patch.
func (s *Store) ShowFor(ctx context.Context, agent, id string, q ShowQuery) (ShowResult, error) {
	var out ShowResult
	err := s.withHistory(agent, id, q.Path, func(g *localGit, p Project, spec []string) error {
		rev, err := g.resolve(ctx, p, q.Revision)
		if err != nil {
			return err
		}
		// Who, when and what the message says is read without the pathspec:
		// it is not file content, and the snapshot revision itself is a commit
		// the model was already handed. Everything below that does carry file
		// content — the file list and the patch — keeps the scope pinned.
		meta, _, err := g.run(ctx, 1<<16, "show", "--quiet", "--date=iso-strict", "--no-color", "--format="+logFormat, rev, "--")
		if err != nil {
			return err
		}
		commits := parseLog(meta)
		if len(commits) == 0 {
			return fmt.Errorf("读不到这个提交：%s", shortRev(q.Revision))
		}
		out.Commit = commits[0]
		stat, _, err := g.run(ctx, 1<<18, append([]string{"show", "--numstat", "--find-renames", "--no-color", "--format=", rev}, pathspecArgs(spec)...)...)
		if err != nil {
			return err
		}
		out.Commit.Files = parseNumstat(stat)
		out.history = g.state(ctx, p, spec)
		if len(out.Commit.Files) == 0 && len(spec) > 0 {
			// Not an error: the commit is real and its message is right there.
			// It simply did not touch this project's directories, and saying so
			// is the difference between "没改到你能看的地方" and "查不到".
			out.Note = "这个提交没有改动到本项目可分析的目录（" + strings.Join(spec, "、") + "），所以没有文件清单和 diff。" + out.Note
		}
		if q.Patch {
			patch, over, err := g.run(ctx, maxPatchBytes, append([]string{"show", "--patch", "--find-renames", "--unified=3", "--no-color", "--format=", rev}, pathspecArgs(spec)...)...)
			if err != nil && !over {
				return err
			}
			out.Patch, out.Truncated = patch, over
			if over {
				out.Note = truncatedPatchNote + out.Note
			}
		}
		return nil
	})
	return out, err
}

// DiffFor compares two revisions. Both must already be in the local cache,
// which is the shallow depth's most visible edge: "对比半年前" is a sync depth
// question, not a diff question, and the error says which.
func (s *Store) DiffFor(ctx context.Context, agent, id string, q DiffQuery) (DiffResult, error) {
	var out DiffResult
	err := s.withHistory(agent, id, q.Path, func(g *localGit, p Project, spec []string) error {
		from, err := g.resolve(ctx, p, q.From)
		if err != nil {
			return err
		}
		to, err := g.resolve(ctx, p, q.To)
		if err != nil {
			return err
		}
		out.From, out.To = from, to
		stat, _, err := g.run(ctx, 1<<18, append([]string{"diff", "--numstat", "--find-renames", "--no-color", from, to}, pathspecArgs(spec)...)...)
		if err != nil {
			return err
		}
		out.Files = parseNumstat(stat)
		out.history = g.state(ctx, p, spec)
		if q.Patch {
			patch, over, err := g.run(ctx, maxPatchBytes, append([]string{"diff", "--patch", "--find-renames", "--unified=3", "--no-color", from, to}, pathspecArgs(spec)...)...)
			if err != nil && !over {
				return err
			}
			out.Patch, out.Truncated = patch, over
			if over {
				out.Note = truncatedPatchNote + out.Note
			}
		}
		return nil
	})
	return out, err
}

const truncatedPatchNote = "补丁超出上限已截断，请用 path 缩小到具体目录或文件后重读。"

// withHistory is WithRead's equivalent for the object cache: the grant is
// rechecked on every call, the pathspec is pinned to the project's configured
// directories before any git runs, and a project whose cache does not match its
// current URL and branch has no history rather than someone else's.
func (s *Store) withHistory(agent, id, requested string, fn func(*localGit, Project, []string) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps, err := s.read()
	if err != nil {
		return err
	}
	ps = s.hydrate(ps)
	for _, p := range ps {
		if p.ID != id || !allowed(p, agent) {
			continue
		}
		if p.Revision == "" {
			return ErrNoHistory
		}
		cache := s.gitCachePath(id)
		if !identifier.MatchString(id) || !cacheMatches(cache, p) {
			return ErrNoHistory
		}
		spec, err := scopePathspec(p, requested)
		if err != nil {
			return err
		}
		// Shared with other readers, exclusive against the sync's prune and
		// cache replacement. A fetch runs concurrently, which is the point.
		lock := s.cacheLock(id)
		lock.RLock()
		defer lock.RUnlock()
		return fn(&localGit{dir: cache}, p, spec)
	}
	return fmt.Errorf("%w（当前 Agent：%s）", ErrDenied, agent)
}

// cacheMatches repeats fetchCache's identity check. A cache left over from a
// previous repository or branch is not this project's history, and reading it
// would answer questions about the wrong code with no sign that it had.
func cacheMatches(cache string, p Project) bool {
	var state gitCacheState
	b, err := os.ReadFile(filepath.Join(cache, "codeproject.json"))
	if err != nil {
		return false
	}
	return json.Unmarshal(b, &state) == nil && state.URL == p.URL && state.Branch == p.Branch
}

// historyHead is the commit the published snapshot was built from, not the
// cache's ref. A sync that fetched and then failed to export leaves the ref
// ahead of the snapshot, and history for code nobody can read is a trap.
func historyHead(p Project) string {
	if p.Revision != "" {
		return p.Revision
	}
	return "refs/heads/snapshot"
}

// scopePathspec turns the project's configured directories, and the caller's
// optional narrowing, into git pathspecs. This is the range limit for history:
// without it a diff would happily print the contents of every file in the
// repository, including the directories the read tools refuse to open.
func scopePathspec(p Project, requested string) ([]string, error) {
	var scope []string
	whole := false
	for _, d := range p.Scope() {
		d = path.Clean(strings.TrimSpace(strings.ReplaceAll(d, "\\", "/")))
		if d == "" || d == "." {
			whole = true
			continue
		}
		if d == ".." || strings.HasPrefix(d, "../") || path.IsAbs(d) {
			continue
		}
		scope = append(scope, d)
	}
	if whole {
		scope = nil
	}
	requested = strings.TrimSpace(strings.ReplaceAll(requested, "\\", "/"))
	if requested == "" {
		return scope, nil
	}
	clean := path.Clean(requested)
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return nil, fmt.Errorf("请使用项目内相对路径")
	}
	if clean == "." {
		return scope, nil
	}
	if !whole && !underScope(clean, scope) {
		return nil, fmt.Errorf("路径越界，只能查看已配置目录 %v 的历史：%s", scope, requested)
	}
	return []string{clean}, nil
}

func underScope(rel string, scope []string) bool {
	for _, s := range scope {
		if rel == s || strings.HasPrefix(rel, s+"/") || strings.HasPrefix(s, rel+"/") {
			return true
		}
	}
	return false
}

// pathspecArgs renders the scope after the "--" separator. :(literal) turns off
// pathspec magic so a directory named with a glob character means that
// directory, and never a pattern the repository chose.
func pathspecArgs(spec []string) []string {
	out := []string{"--"}
	for _, s := range spec {
		out = append(out, ":(literal)"+s)
	}
	return out
}

// localGit runs read-only git inside one project's object cache. It carries no
// credential and no remote: everything it reads is already on disk, which is
// what separates it from gitSession.
type localGit struct{ dir string }

func (g *localGit) command(ctx context.Context, args ...string) *exec.Cmd {
	base := []string{
		"--no-pager", "-C", g.dir,
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "credential.helper=",
		"-c", "protocol.allow=never",
		// Containers mount the data directory from the host, so the cache is
		// routinely owned by a uid git does not recognise as "me". Without
		// this, every history call in Docker fails with "dubious ownership" —
		// which reads to the model as the repository being broken. The path is
		// this process's own data directory, not a user-supplied one.
		"-c", "safe.directory=" + g.dir,
	}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// run executes git and keeps at most max bytes of stdout. Hitting the cap stops
// the command rather than draining a patch nobody will read: over is true and
// the partial output stands, so a 40 MB refactor commit costs a truncation note
// instead of the turn.
func (g *localGit) run(ctx context.Context, max int, args ...string) (out string, over bool, err error) {
	cmd := g.command(ctx, args...)
	stdout := &capWriter{max: max}
	var stderr boundedBuffer
	stderr.max = 4 << 10
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	runErr := cmd.Run()
	if stdout.over {
		return stdout.b.String(), true, nil
	}
	if runErr != nil {
		var missing *exec.Error
		if errors.As(runErr, &missing) {
			return "", false, errors.New("服务端没有安装 git，或它不在 PATH 中：代码历史需要它")
		}
		if ctx.Err() != nil {
			return "", false, errors.New("读取代码历史超时")
		}
		detail := excerpt(stderr.b.String())
		if detail == "" {
			detail = runErr.Error()
		}
		return "", false, fmt.Errorf("读取代码历史失败：%s", detail)
	}
	return stdout.b.String(), false, nil
}

// resolve validates a caller-supplied revision and turns it into a full object
// id that exists in the cache. The shallow boundary is the interesting failure:
// a commit that is simply older than the fetch depth is not a bad argument, and
// telling the model "没有这个提交" would make it doubt what the log just told it.
func (g *localGit) resolve(ctx context.Context, p Project, rev string) (string, error) {
	rev = strings.TrimSpace(rev)
	if rev == "" || strings.EqualFold(rev, "HEAD") {
		rev = historyHead(p)
	} else {
		if !revisionArg.MatchString(rev) {
			return "", fmt.Errorf("revision 只接受提交 id（可带 ~n 或 ^），从 log_project_commits 的结果里取：%s", rev)
		}
		if strings.HasPrefix(strings.ToUpper(rev), "HEAD") {
			rev = historyHead(p) + rev[4:]
		}
	}
	// --quiet's contract is an empty stdout and a non-zero exit for a revision
	// the repository does not have, so the exit status is not the interesting
	// part here — only whether git ran at all.
	cmd := g.command(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	b, runErr := cmd.Output()
	var missing *exec.Error
	if errors.As(runErr, &missing) {
		return "", errors.New("服务端没有安装 git，或它不在 PATH 中：代码历史需要它")
	}
	if ctx.Err() != nil {
		return "", errors.New("读取代码历史超时")
	}
	if out := strings.TrimSpace(string(b)); out != "" {
		return out, nil
	}
	return "", fmt.Errorf("本地历史里没有这个版本：%s。本地只保留最近 %d 个提交。要看更早的，请用户在代码项目设置里调大「保留提交历史」再同步一次",
		shortRev(rev), g.localCommits(ctx, p))
}

// state is the provenance every result carries.
func (g *localGit) state(ctx context.Context, p Project, spec []string) history {
	h := history{Revision: p.Revision, Scope: spec, Depth: g.localCommits(ctx, p)}
	if out, _, err := g.run(ctx, 64, "rev-parse", "--is-shallow-repository"); err == nil {
		h.Shallow = strings.TrimSpace(out) == "true"
	}
	if h.Shallow {
		h.Note = fmt.Sprintf("本地只有最近 %d 个提交的历史（由项目的「保留提交历史」设置决定），更早的提交不在本地——提交数偏少是同步深度造成的，不代表仓库就这么多提交。", h.Depth)
	}
	return h
}

func (g *localGit) localCommits(ctx context.Context, p Project) int {
	out, _, err := g.run(ctx, 64, "rev-list", "--count", historyHead(p))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// logFormat separates fields with 0x1f and records with 0x1e so a subject
// containing a tab, or a body containing blank lines, cannot be mistaken for
// structure. --name-status output follows the last field of each record.
const logFormat = "%x1e%H%x1f%an%x1f%ad%x1f%s%x1f%b%x1f"

func parseLog(raw string) []Commit {
	var out []Commit
	for _, record := range strings.Split(raw, "\x1e") {
		if strings.TrimSpace(record) == "" {
			continue
		}
		fields := strings.SplitN(record, "\x1f", 6)
		if len(fields) < 5 {
			continue
		}
		c := Commit{
			Revision: strings.TrimSpace(fields[0]),
			Author:   strings.TrimSpace(fields[1]),
			Date:     strings.TrimSpace(fields[2]),
			Subject:  strings.TrimSpace(fields[3]),
			Body:     strings.TrimSpace(fields[4]),
		}
		if len(fields) == 6 {
			c.Files = parseNameStatus(fields[5])
		}
		out = append(out, c)
	}
	return out
}

// parseNameStatus reads "M\tpath" and "R100\told\tnew".
func parseNameStatus(block string) []FileChange {
	var out []FileChange
	for _, line := range strings.Split(block, "\n") {
		parts := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		fc := FileChange{Status: parts[0], Path: parts[1]}
		if len(parts) > 2 && parts[2] != "" {
			fc.OldPath, fc.Path = parts[1], parts[2]
		}
		if out = append(out, fc); len(out) >= maxFilesListed {
			break
		}
	}
	return out
}

// parseNumstat reads "12\t3\tpath", with "-" counts for binary files. Renames
// arrive rendered as "dir/{old => new}.go"; the rendering is kept as-is rather
// than half-decoded into a path that does not exist on either side.
func parseNumstat(raw string) []FileChange {
	var out []FileChange
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 3)
		if len(parts) < 3 || parts[2] == "" {
			continue
		}
		fc := FileChange{Path: parts[2]}
		if parts[0] == "-" || parts[1] == "-" {
			fc.Binary = true
		} else {
			fc.Added, _ = strconv.Atoi(parts[0])
			fc.Deleted, _ = strconv.Atoi(parts[1])
		}
		if out = append(out, fc); len(out) >= maxFilesListed {
			break
		}
	}
	return out
}

// capWriter keeps at most max bytes and then fails the write, which is how the
// producing git process is asked to stop.
type capWriter struct {
	b    bytes.Buffer
	max  int
	over bool
}

var errCapped = errors.New("output cap reached")

func (w *capWriter) Write(p []byte) (int, error) {
	room := w.max - w.b.Len()
	if len(p) <= room {
		w.b.Write(p)
		return len(p), nil
	}
	if room > 0 {
		w.b.Write(p[:room])
	}
	w.over = true
	return 0, errCapped
}

// shortRev abbreviates an object id for a message. The tool layer has its own
// copy for the stale-snapshot gate; this one is here so the store does not
// depend on it.
func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	if rev == "" {
		return "无"
	}
	return rev
}
