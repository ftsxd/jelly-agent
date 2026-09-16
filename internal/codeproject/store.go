// Package codeproject manages operator-controlled code snapshots and live grants.
package codeproject

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("项目不存在")
var ErrDenied = errors.New("项目不存在、未授权或授权已过期")
var ErrBusy = errors.New("项目正在拉取，请稍后再操作")
var ErrProbeOff = errors.New("远端核对已关闭（files.code_projects.no_remote_check）")
var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// imeHint names the likeliest cause when an ASCII-only field holds something
// else. On a Chinese-language console the input method is on by default, and a
// full-width ｘ is visually almost identical to an ASCII x — "invalid" alone
// sends people looking at the wrong thing entirely.
//
// The value itself is never echoed: this field holds a variable NAME, but
// someone will eventually paste the token into it, and an error message is not
// where a secret should surface.
func imeHint(v string) string {
	for _, r := range v {
		if r > 127 {
			return "（检测到全角或中文字符，请把输入法切到英文半角后重新输入）"
		}
	}
	return ""
}

type Grant struct {
	Agent     string     `json:"agent"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// DirectoryInfo is the human-maintained business context for one configured
// directory. It is what lets an agent turn "the order service" into
// services/order without the user restating the path every time.
//
// It is NOT evidence: the description is whatever the operator typed, never
// something verified against the code, and it never widens what may be read.
type DirectoryInfo struct {
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// Empty reports an annotation with nothing in it, which is stored as no
// annotation at all rather than as an empty row.
func (d DirectoryInfo) Empty() bool {
	return d.Name == "" && d.Description == "" && len(d.Tags) == 0
}

// Directory is one configured directory as handed to the agent: the path, its
// role in the project, and the operator's description of it.
type Directory struct {
	Path        string   `json:"path"`
	Role        string   `json:"role"` // "main", "reference", or "annotated"
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// Sync states as persisted in projects.json. The in-memory busy map alone was
// not enough: a restart during a pull left a project looking idle forever.
const (
	SyncQueued      = "queued"
	SyncRunning     = "running"
	SyncSucceeded   = "succeeded"
	SyncFailed      = "failed"
	SyncInterrupted = "interrupted"
)

// SyncRequest is an agent asking for the snapshot to be refreshed.
//
// It is a request and not a sync: pulling uses the stored credential, takes
// minutes on a monorepo and replaces the tree every later answer cites, so the
// decision stays with a person. The agent's job is to notice that the snapshot
// is behind and say so with the evidence it saw; the code page turns that into
// one click.
type SyncRequest struct {
	Agent  string `json:"agent"`
	Reason string `json:"reason,omitempty"`
	// Remote is the head the agent saw when it asked, so the page can show
	// what it would be syncing to rather than "something newer".
	Remote      string    `json:"remote_revision,omitempty"`
	Local       string    `json:"local_revision,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
}

type Project struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	URL      string  `json:"url"`
	Branch   string  `json:"branch"`
	Username string  `json:"username,omitempty"`
	TokenEnv string  `json:"token_env,omitempty"`
	Grants   []Grant `json:"grants"`

	// RootPath is the main analysis directory, repo-relative. "." (the zero
	// value's meaning) is the whole repository, which is what every project
	// created before this field existed gets.
	RootPath string `json:"root_path,omitempty"`
	// ReferencePaths are additional repo-relative directories the agent may
	// read — a shared common/ or another service's proto directory.
	ReferencePaths []string `json:"reference_paths,omitempty"`
	// DirectoryMeta is keyed by normalized repo-relative path. Any directory
	// inside the analysis scope may be annotated — the whole point is to label
	// the 130 services under a whole-repo root, not just the root itself.
	DirectoryMeta map[string]DirectoryInfo `json:"directory_metadata,omitempty"`
	// DirectoryDrafts holds annotations an agent proposed but nobody has
	// accepted. They are kept apart from DirectoryMeta on purpose: a
	// description the model wrote about code it skimmed is a suggestion, and
	// presenting it back to the model as established fact would launder a guess
	// into the record. Only DirectoryMeta reaches the read tools.
	DirectoryDrafts map[string]DirectoryInfo `json:"directory_drafts,omitempty"`
	// DirIssues lists configured directories missing from the last snapshot.
	// A project with issues is not presented as ready to analyse.
	DirIssues []string `json:"dir_issues,omitempty"`

	Revision  string     `json:"revision,omitempty"`
	SyncedAt  *time.Time `json:"synced_at,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	Snapshot  string     `json:"snapshot,omitempty"`

	// Annotate tracks a console-triggered run where the assigned agent reads
	// the repository and proposes labels. Kept beside the sync state because it
	// is the same shape of thing: a long background job the page follows.
	Annotate *AnnotateRun `json:"annotate,omitempty"`

	// SyncRequest is a pending ask from an agent, cleared by approving it,
	// rejecting it, or by any sync that succeeds — at which point the thing it
	// asked for has happened, however it was triggered.
	SyncRequest *SyncRequest `json:"sync_request,omitempty"`

	SyncState     string     `json:"sync_state,omitempty"`
	SyncTaskID    string     `json:"sync_task_id,omitempty"`
	SyncStartedAt *time.Time `json:"sync_started_at,omitempty"`

	Syncing bool `json:"syncing,omitempty"`
	// HasToken says a credential is stored, which is all any API returns about
	// it. Transient like Syncing: filled in by List, cleared by Save, never a
	// field the token itself could occupy.
	HasToken bool `json:"has_token,omitempty"`
}

// Directories returns the configured directories in agent-facing form: the main
// analysis directory first, then the references, each with its description.
func (p Project) Directories() []Directory {
	out := []Directory{{Path: p.Main(), Role: "main"}}
	for _, r := range p.ReferencePaths {
		out = append(out, Directory{Path: r, Role: "reference"})
	}
	for i := range out {
		if info, ok := p.DirectoryMeta[out[i].Path]; ok {
			out[i].Name, out[i].Description, out[i].Tags = info.Name, info.Description, info.Tags
		}
	}
	return out
}

// Annotations returns every annotated directory, sorted by path, including the
// ones that are not configured entry points. This is the catalogue the console
// edits and the tools search; Directories stays the short list of where an
// agent may actually start reading.
func (p Project) Annotations() []Directory {
	out := make([]Directory, 0, len(p.DirectoryMeta))
	configured := map[string]string{p.Main(): "main"}
	for _, r := range p.ReferencePaths {
		configured[r] = "reference"
	}
	for path, info := range p.DirectoryMeta {
		role := configured[path]
		if role == "" {
			role = "annotated"
		}
		out = append(out, Directory{Path: path, Role: role, Name: info.Name, Description: info.Description, Tags: info.Tags})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Annotation looks up one directory's labels by repo-relative path.
func (p Project) Annotation(rel string) (DirectoryInfo, bool) {
	info, ok := p.DirectoryMeta[rel]
	return info, ok
}

// Main is RootPath with the legacy empty value read as "the whole repository".
func (p Project) Main() string {
	if strings.TrimSpace(p.RootPath) == "" {
		return "."
	}
	return p.RootPath
}

// Scope is the prefix allowlist handed to the file tools: every directory the
// project may read, main and references together.
func (p Project) Scope() []string {
	return append([]string{p.Main()}, p.ReferencePaths...)
}

// AnnotateRun is the state of one label-generation run.
type AnnotateRun struct {
	State     string     `json:"state,omitempty"` // running | succeeded | failed
	TaskID    string     `json:"task_id,omitempty"`
	Agent     string     `json:"agent,omitempty"`
	Session   string     `json:"session,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	Error     string     `json:"error,omitempty"`
	Proposed  int        `json:"proposed,omitempty"`
}

// Limits bounds one sync. Zero fields fall back to DefaultLimits, so a Store
// opened without configuration still behaves sanely.
type Limits struct {
	SyncTimeout      time.Duration
	MaxSnapshotBytes int64
	MaxSnapshotFiles int
}

// DefaultLimits is sized for a full microservice monorepo checkout.
var DefaultLimits = Limits{SyncTimeout: 30 * time.Minute, MaxSnapshotBytes: 2 << 30, MaxSnapshotFiles: 200000}

type Store struct {
	dir     string
	mu      sync.RWMutex
	busy    map[string]string // project id -> running sync task id
	limits  Limits
	recover sync.Once

	// probes caches the last "is the remote ahead" answer per project. An
	// agent asks at the start of every analysis and several agents share one
	// store, so without this a busy afternoon is one ls-remote per turn
	// against someone's git server. Guarded separately from mu: it is not part
	// of what projects.json says.
	probeMu   sync.Mutex
	probes    map[string]remoteProbe
	autoProbe bool
}

type remoteProbe struct {
	st  RemoteStatus
	err error
	at  time.Time
}

// SetLimits applies configuration to the shared Store. Open returns the same
// instance across engine reloads, so this is called on every lookup rather than
// only at construction.
func (s *Store) SetLimits(l Limits) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.SyncTimeout > 0 {
		s.limits.SyncTimeout = l.SyncTimeout
	}
	if l.MaxSnapshotBytes > 0 {
		s.limits.MaxSnapshotBytes = l.MaxSnapshotBytes
	}
	if l.MaxSnapshotFiles > 0 {
		s.limits.MaxSnapshotFiles = l.MaxSnapshotFiles
	}
}

// SetAutoProbe decides whether the agent-facing freshness check may reach the
// network. Off until the engine turns it on, so a store opened by a test does
// not shell out to git against whatever URL the fixture invented — and an
// operator on a closed network can turn it off in config without losing the
// console's explicit 检查更新 button.
func (s *Store) SetAutoProbe(on bool) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	s.autoProbe = on
}

var stores sync.Map

// Open shares locks across engine reloads. One server process owns a directory.
func Open(dir string) *Store {
	dir, _ = filepath.Abs(dir)
	s, _ := stores.LoadOrStore(dir, &Store{dir: dir, busy: map[string]string{}, limits: DefaultLimits})
	st := s.(*Store)
	st.recover.Do(func() {
		st.markInterrupted()
		st.migrateAnnotations()
	})
	return st
}

// markInterrupted runs once per process. A pull that was in flight when the
// service stopped left "queued"/"running" on disk with no goroutine behind it;
// without this the project shows as syncing forever and the retry button stays
// disabled. First version only marks — no resumable checkpoint.
func (s *Store) markInterrupted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := s.read()
	if err != nil {
		return
	}
	changed := false
	for i := range ps {
		if ps[i].SyncState == SyncQueued || ps[i].SyncState == SyncRunning {
			ps[i].SyncState = SyncInterrupted
			ps[i].SyncTaskID = ""
			ps[i].LastError = "服务重启，同步已中断，可重新同步"
			changed = true
		}
	}
	if changed {
		_ = s.write(ps)
	}
}
func (s *Store) Dir() string { return s.dir }
func (s *Store) read() ([]Project, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, "projects.json"))
	if errors.Is(err, os.ErrNotExist) {
		return []Project{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ps []Project
	if err := json.Unmarshal(b, &ps); err != nil {
		return nil, err
	}
	return ps, nil
}
func (s *Store) write(ps []Project) error {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	// The catalogue lives in annotations.json, keyed by repository. Projects
	// carry a hydrated copy in memory for every reader downstream; persisting it
	// here would fork the two and reintroduce exactly the per-project drift this
	// storage split exists to remove.
	ps = append([]Project(nil), ps...)
	for i := range ps {
		ps[i].DirectoryMeta, ps[i].DirectoryDrafts = nil, nil
	}
	b, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".metadata-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(s.dir, "projects.json"))
}
func (s *Store) List() ([]Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps, err := s.read()
	secrets, _ := s.readSecrets()
	for i := range ps {
		ps[i].Syncing = s.busy[ps[i].ID] != ""
		ps[i].HasToken = secrets[ps[i].ID] != ""
	}
	return s.hydrate(ps), err
}
func Validate(p Project) error {
	if !identifier.MatchString(p.ID) {
		return fmt.Errorf("项目标识只能包含字母、数字、下划线和连字符，长度 1–80")
	}
	if strings.TrimSpace(p.Name) == "" || len(p.Name) > 200 {
		return fmt.Errorf("请填写项目名称（最多 200 字节）")
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(p.URL, "\r\n\t ") {
		return fmt.Errorf("仓库地址必须是 HTTPS URL，不能携带账号、密码或查询参数")
	}
	if p.Branch == "" || len(p.Branch) > 200 || strings.HasPrefix(p.Branch, "-") || strings.ContainsAny(p.Branch, " ~^:?*[\\\r\n\t") || strings.Contains(p.Branch, "..") || strings.Contains(p.Branch, "@{") || strings.HasSuffix(p.Branch, "/") || strings.HasSuffix(p.Branch, ".") {
		return fmt.Errorf("请填写有效的分支名称%s", imeHint(p.Branch))
	}
	if p.TokenEnv != "" && !envName.MatchString(p.TokenEnv) {
		return fmt.Errorf("凭据变量名只能包含字母、数字和下划线，且不能以数字开头%s", imeHint(p.TokenEnv))
	}
	if strings.ContainsAny(p.Username, "\r\n:") {
		return fmt.Errorf("Git 用户名不能包含冒号或换行")
	}
	seen := map[string]bool{}
	for _, g := range p.Grants {
		if !identifier.MatchString(g.Agent) || seen[g.Agent] {
			return fmt.Errorf("授权 Agent 无效或重复")
		}
		seen[g.Agent] = true
	}
	return validateDirs(p)
}

// CleanDir normalizes a configured directory to a repo-relative slash path, or
// reports why it cannot be one. The repository is attacker-influenced content,
// so an absolute path, a traversal, or a Windows separator is rejected here
// rather than left for the file tools to catch later.
func CleanDir(raw string) (string, error) {
	d := strings.TrimSpace(raw)
	if d == "" {
		return "", fmt.Errorf("目录不能为空")
	}
	if strings.ContainsAny(d, "\\\r\n\t") {
		return "", fmt.Errorf("目录路径不能包含反斜杠或换行：%s", raw)
	}
	if path.IsAbs(d) || filepath.IsAbs(d) {
		return "", fmt.Errorf("请使用仓库内相对路径，不能是绝对路径：%s", raw)
	}
	d = path.Clean(d)
	if d == "." {
		return ".", nil
	}
	if d == ".." || strings.HasPrefix(d, "../") {
		return "", fmt.Errorf("目录路径越界，必须在仓库之内：%s", raw)
	}
	return strings.TrimSuffix(d, "/"), nil
}

// validateDirs checks the directory configuration and, importantly, that every
// directory_metadata key names a directory that is actually configured.
// Metadata is description only: it must never be a second way to name a path.
func validateDirs(p Project) error {
	root, err := CleanDir(p.Main())
	if err != nil {
		return fmt.Errorf("主分析目录无效：%w", err)
	}
	configured := map[string]bool{root: true}
	for _, r := range p.ReferencePaths {
		c, err := CleanDir(r)
		if err != nil {
			return fmt.Errorf("参考目录无效：%w", err)
		}
		if c == root {
			return fmt.Errorf("参考目录与主分析目录重复：%s", c)
		}
		if configured[c] {
			return fmt.Errorf("参考目录重复：%s", c)
		}
		configured[c] = true
	}
	if len(p.ReferencePaths) > 50 {
		return fmt.Errorf("参考目录最多 50 个")
	}
	scope := make([]string, 0, len(configured))
	for c := range configured {
		scope = append(scope, c)
	}
	if err := validateAnnotations(p.DirectoryMeta, scope, "目录说明"); err != nil {
		return err
	}
	return validateAnnotations(p.DirectoryDrafts, scope, "目录说明草稿")
}

// maxAnnotations bounds one project's annotation set. A 130-service monorepo
// wants every service labelled; a runaway generator does not get to write a
// million rows into the config file.
const maxAnnotations = 5000

// validateAnnotations checks that every annotated path is INSIDE the project's
// analysis scope.
//
// The earlier rule — an annotation may only name a configured directory — was
// too tight to be useful: a whole-repo project has exactly one configured
// directory, so it could hold exactly one label for 130 services. Scope
// containment is the property that actually matters, and it still holds: an
// annotation on services/order when the root is "." names something already
// readable, so it widens nothing. One outside the scope is refused, because
// that WOULD be a second way to name a path.
func validateAnnotations(meta map[string]DirectoryInfo, scope []string, label string) error {
	if len(meta) > maxAnnotations {
		return fmt.Errorf("%s最多 %d 条", label, maxAnnotations)
	}
	for key, info := range meta {
		clean, err := CleanDir(key)
		if err != nil {
			return fmt.Errorf("%s的路径无效：%w", label, err)
		}
		if clean != key {
			return fmt.Errorf("%s的路径未规范化：%s", label, key)
		}
		if !withinScope(clean, scope) {
			return fmt.Errorf("%s只能标注分析范围内的目录：%s", label, key)
		}
		if len(info.Name) > 200 {
			return fmt.Errorf("目录业务名称最多 200 字节：%s", key)
		}
		if len(info.Description) > 2000 {
			return fmt.Errorf("目录业务描述最多 2000 字节：%s", key)
		}
		if len(info.Tags) > 20 {
			return fmt.Errorf("单个目录最多 20 个标签：%s", key)
		}
		seen := map[string]bool{}
		for _, tag := range info.Tags {
			if tag == "" || len(tag) > 60 {
				return fmt.Errorf("标签不能为空且最多 60 字节：%s", key)
			}
			if strings.ContainsAny(tag, "\r\n\t") {
				return fmt.Errorf("标签不能包含换行：%s", key)
			}
			if seen[tag] {
				return fmt.Errorf("标签重复：%s / %s", key, tag)
			}
			seen[tag] = true
		}
	}
	return nil
}

// withinScope reports whether a repo-relative path is at or below one of the
// project's configured directories. "." as a scope entry covers everything.
func withinScope(rel string, scope []string) bool {
	for _, dir := range scope {
		if dir == "." || rel == dir || strings.HasPrefix(rel, dir+"/") {
			return true
		}
	}
	return false
}

// Normalize cleans the directory configuration in place so what is stored is
// what the tools compare against. Callers run it before Validate.
func (p *Project) Normalize() error {
	root, err := CleanDir(p.Main())
	if err != nil {
		return fmt.Errorf("主分析目录无效：%w", err)
	}
	p.RootPath = root
	refs := []string{}
	for _, r := range p.ReferencePaths {
		c, err := CleanDir(r)
		if err != nil {
			return fmt.Errorf("参考目录无效：%w", err)
		}
		refs = append(refs, c)
	}
	p.ReferencePaths = refs
	meta, err := normalizeAnnotations(p.DirectoryMeta)
	if err != nil {
		return err
	}
	drafts, err := normalizeAnnotations(p.DirectoryDrafts)
	if err != nil {
		return err
	}
	p.DirectoryMeta, p.DirectoryDrafts = meta, drafts
	return nil
}

// normalizeAnnotations cleans paths and trims labels so what is stored is what
// the tools compare against. An annotation left completely blank is dropped
// rather than kept as an empty row.
func normalizeAnnotations(in map[string]DirectoryInfo) (map[string]DirectoryInfo, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := map[string]DirectoryInfo{}
	for k, v := range in {
		c, err := CleanDir(k)
		if err != nil {
			return nil, fmt.Errorf("目录说明的路径无效：%w", err)
		}
		v.Name, v.Description = strings.TrimSpace(v.Name), strings.TrimSpace(v.Description)
		tags := []string{}
		seen := map[string]bool{}
		for _, tag := range v.Tags {
			tag = strings.TrimSpace(tag)
			if tag == "" || seen[tag] {
				continue
			}
			seen[tag] = true
			tags = append(tags, tag)
		}
		sort.Strings(tags)
		v.Tags = tags
		if len(tags) == 0 {
			v.Tags = nil
		}
		if v.Empty() {
			continue
		}
		out[c] = v
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
func (s *Store) Save(p Project) error {
	if err := p.Normalize(); err != nil {
		return err
	}
	if err := Validate(p); err != nil {
		return err
	}
	// The form carries labels for the configured directories only; they are
	// applied to the repository catalogue after the project is stored. The rest
	// of the catalogue — a whole-repo project may hold one per service — is not
	// in the form at all and must survive an unrelated edit.
	fromForm := p.DirectoryMeta
	p.clearSnapshot()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[p.ID] != "" {
		return ErrBusy
	}
	ps, err := s.read()
	if err != nil {
		return err
	}
	for i, old := range ps {
		if old.ID == p.ID {
			p.Grants = old.Grants // Configuration edits must not undo a concurrent grant update.
			fromForm = p.DirectoryMeta
			// Repository identity changes revoke access to the old snapshot.
			// Directory configuration changes deliberately do NOT: the snapshot
			// holds the whole repository, so renaming a directory or editing a
			// description is a metadata edit that needs no re-pull.
			if old.URL == p.URL && old.Branch == p.Branch {
				p.Revision = old.Revision
				p.Snapshot = old.Snapshot
				p.SyncedAt = old.SyncedAt
				p.LastError = old.LastError
				p.SyncState = old.SyncState
				p.SyncTaskID = old.SyncTaskID
				p.SyncStartedAt = old.SyncStartedAt
				if p.Snapshot != "" {
					// New directories are checked against the snapshot we
					// already have, so a typo surfaces on save, not on the
					// agent's first empty search.
					p.DirIssues = checkDirs(filepath.Join(s.dir, p.Snapshot), p)
				}
			}
			ps[i] = p
			if err := s.write(ps); err != nil {
				return err
			}
			if p.Snapshot != old.Snapshot {
				s.removeSnapshot(old.Snapshot)
			}
			return s.applyFormAnnotations(p, fromForm)
		}
	}
	p.clearSnapshot()
	if err := s.write(append(ps, p)); err != nil {
		return err
	}
	return s.applyFormAnnotations(p, fromForm)
}

// applyFormAnnotations writes the labels the project form owns into the
// repository catalogue, leaving every other entry alone.
func (s *Store) applyFormAnnotations(p Project, fromForm map[string]DirectoryInfo) error {
	owned := p.configuredDirs()
	a, err := s.readAnnotations()
	if err != nil {
		return err
	}
	key := RepoKey(p.URL)
	if a.Annotations == nil {
		a.Annotations = map[string]map[string]DirectoryInfo{}
	}
	cat := a.Annotations[key]
	if cat == nil {
		cat = map[string]DirectoryInfo{}
	}
	// Apply everything the caller sent — the console form only ever carries the
	// configured directories, but the API is also how a catalogue gets seeded,
	// and silently dropping paths it asked for would be data loss.
	for path, info := range fromForm {
		if info.Empty() {
			delete(cat, path)
			continue
		}
		cat[path] = info
	}
	// A configured directory the form omitted means its row was cleared. Every
	// other entry is not the form's to touch.
	for path := range owned {
		if _, sent := fromForm[path]; !sent {
			delete(cat, path)
		}
	}
	a.Annotations[key] = cat
	return s.writeAnnotations(a)
}

func (p *Project) clearSnapshot() {
	p.Revision = ""
	p.Snapshot = ""
	p.SyncedAt = nil
	p.LastError = ""
	p.Syncing = false
	p.SyncState = ""
	p.SyncTaskID = ""
	p.SyncStartedAt = nil
	p.DirIssues = nil
	p.HasToken = false
}
func (s *Store) SetGrants(id string, grants []Grant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := s.read()
	if err != nil {
		return err
	}
	for i := range ps {
		if ps[i].ID == id {
			ps[i].Grants = grants
			if err := Validate(ps[i]); err != nil {
				return err
			}
			return s.write(ps)
		}
	}
	return ErrNotFound
}
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[id] != "" {
		return ErrBusy
	}
	ps, err := s.read()
	if err != nil {
		return err
	}
	for i, p := range ps {
		if p.ID == id {
			remaining := make([]Project, 0, len(ps)-1)
			remaining = append(remaining, ps[:i]...)
			remaining = append(remaining, ps[i+1:]...)
			if err := s.write(remaining); err != nil {
				return err
			}
			s.removeSnapshot(p.Snapshot)
			// The credential has no owner left; leaving it behind would let a
			// project recreated under the same id silently inherit it.
			_ = s.setTokenLocked(id, "")
			// Labels describe the repository, so they outlive one project over
			// it — but not the last one.
			s.dropRepoAnnotations(remaining, p.URL)
			return nil
		}
	}
	return ErrNotFound
}
func allowed(p Project, agent string) bool {
	for _, g := range p.Grants {
		if g.Agent == agent && (g.ExpiresAt == nil || time.Now().Before(*g.ExpiresAt)) {
			return true
		}
	}
	return false
}
func (s *Store) Visible(agent string) ([]Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps, err := s.read()
	if err != nil {
		return nil, err
	}
	ps = s.hydrate(ps)
	out := []Project{}
	for _, p := range ps {
		if allowed(p, agent) {
			// Copy only what an agent may see: never the snapshot directory
			// name, the credential variable, or the grant list.
			out = append(out, Project{
				ID: p.ID, Name: p.Name, Branch: p.Branch,
				Revision: p.Revision, SyncedAt: p.SyncedAt,
				RootPath: p.Main(), ReferencePaths: p.ReferencePaths,
				DirectoryMeta: p.DirectoryMeta, DirIssues: p.DirIssues,
				// A pull already under way is the one case where "落后于远端"
				// needs no decision from anyone: it is being fixed.
				SyncState: p.SyncState,
			})
		}
	}
	return out, nil
}

// WithRead rechecks the grant on every call, including calls by an old engine.
// The lock prevents a revoke from racing past an already authorized file read.
func (s *Store) WithRead(agent, id string, fn func(dir string, p Project) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps, err := s.read()
	if err != nil {
		return err
	}
	ps = s.hydrate(ps)
	for _, p := range ps {
		if p.ID == id && allowed(p, agent) {
			if p.Snapshot == "" {
				return fmt.Errorf("项目尚未成功拉取")
			}
			if filepath.Base(p.Snapshot) != p.Snapshot || !strings.HasPrefix(p.Snapshot, "snapshot-") {
				return fmt.Errorf("代码快照无效")
			}
			return fn(filepath.Join(s.dir, p.Snapshot), p)
		}
	}
	// The identity is named because the merged message hides the one thing a
	// denial never tells anyone: which agent was asked about. A project read
	// from inside an agent tree is checked against the node's own name, and
	// "not granted to CodeAnalyzer" is a different afternoon from "not granted
	// to whoever you thought was asking". Existence stays merged in — naming
	// the caller is not the same as confirming the project is there.
	return fmt.Errorf("%w（当前 Agent：%s）", ErrDenied, agent)
}

func (s *Store) removeSnapshot(name string) {
	if filepath.Base(name) == name && strings.HasPrefix(name, "snapshot-") {
		_ = os.RemoveAll(filepath.Join(s.dir, name))
	}
}

// RevokeAgent prevents deleting/recreating an agent name from inheriting grants.
func (s *Store) RevokeAgent(agent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := s.read()
	if err != nil {
		return err
	}
	changed := false
	for i := range ps {
		grants := []Grant{}
		for _, g := range ps[i].Grants {
			if g.Agent == agent {
				changed = true
			} else {
				grants = append(grants, g)
			}
		}
		ps[i].Grants = grants
	}
	if !changed {
		return nil
	}
	return s.write(ps)
}

// configuredDirs is the set of directories the project form owns labels for.
func (p Project) configuredDirs() map[string]bool {
	own := map[string]bool{p.Main(): true}
	for _, r := range p.ReferencePaths {
		own[r] = true
	}
	return own
}

// SetAnnotation labels one directory. Editing one row must not require sending
// the whole catalogue back, which for a monorepo would mean a few hundred
// kilobytes on every keystroke-sized change.
func (s *Store) SetAnnotation(id, dir string, info DirectoryInfo) error {
	return s.editAnnotations(id, func(_ Project, ann, _ map[string]DirectoryInfo) error {
		clean, err := CleanDir(dir)
		if err != nil {
			return err
		}
		if info.Empty() {
			delete(ann, clean)
		} else {
			ann[clean] = info
		}
		return nil
	})
}

// ProposeAnnotations records agent-written labels as drafts. They stay out of
// DirectoryMeta until a person accepts them, so a description the model guessed
// from a skim never reaches the read tools as established fact.
func (s *Store) ProposeAnnotations(id string, proposed map[string]DirectoryInfo) (int, error) {
	n := 0
	err := s.editAnnotations(id, func(_ Project, _, drafts map[string]DirectoryInfo) error {
		for dir, info := range proposed {
			clean, err := CleanDir(dir)
			if err != nil {
				return err
			}
			if info.Empty() {
				continue
			}
			drafts[clean] = info
			n++
		}
		return nil
	})
	return n, err
}

// ResolveDrafts accepts drafts into the catalogue or discards them. A nil
// accept/reject pair with all=true accepts everything pending.
func (s *Store) ResolveDrafts(id string, accept, reject []string, acceptAll bool) (int, error) {
	n := 0
	err := s.editAnnotations(id, func(p Project, ann, drafts map[string]DirectoryInfo) error {
		scope := p.Scope()
		take := func(dir string) {
			// Only what this project may see: accepting from one project must
			// not promote a draft covering a directory outside its scope.
			if info, ok := drafts[dir]; ok && withinScope(dir, scope) {
				ann[dir] = info
				delete(drafts, dir)
				n++
			}
		}
		if acceptAll {
			for dir := range drafts {
				take(dir)
			}
		}
		for _, dir := range accept {
			if clean, err := CleanDir(dir); err == nil {
				take(clean)
			}
		}
		for _, dir := range reject {
			if clean, err := CleanDir(dir); err == nil && withinScope(clean, scope) {
				delete(drafts, clean)
			}
		}
		return nil
	})
	return n, err
}

// mutate applies fn to one project under the write lock, then validates and
// persists. Sync state and the snapshot are never touched.
func (s *Store) mutate(id string, fn func(*Project) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := s.read()
	if err != nil {
		return err
	}
	for i := range ps {
		if ps[i].ID != id {
			continue
		}
		p := ps[i]
		if err := fn(&p); err != nil {
			return err
		}
		if err := p.Normalize(); err != nil {
			return err
		}
		if err := Validate(p); err != nil {
			return err
		}
		ps[i] = p
		return s.write(ps)
	}
	return ErrNotFound
}

// SetAnnotateRun records the state of a label-generation run.
func (s *Store) SetAnnotateRun(id string, run *AnnotateRun) error {
	return s.mutate(id, func(p *Project) error {
		p.Annotate = run
		return nil
	})
}

// RequestSync records an agent's ask that this project be pulled again.
//
// The grant is checked here rather than trusted from the caller: this is the
// one project method an agent reaches that writes anything, and an agent that
// may not read a project may not put a card on its page either.
//
// A second request replaces the first. Two agents asking for the same pull is
// one decision for the operator, not a queue.
func (s *Store) RequestSync(agent, id string, req SyncRequest) error {
	s.mu.RLock()
	ps, err := s.read()
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	ok := false
	for _, p := range ps {
		if p.ID == id && allowed(p, agent) {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("%w（当前 Agent：%s）", ErrDenied, agent)
	}
	req.Agent = agent
	req.RequestedAt = time.Now().UTC()
	return s.mutate(id, func(p *Project) error {
		p.SyncRequest = &req
		return nil
	})
}

// ClearSyncRequest drops the pending ask, whether it was approved or refused.
func (s *Store) ClearSyncRequest(id string) error {
	return s.mutate(id, func(p *Project) error {
		p.SyncRequest = nil
		return nil
	})
}

// AnnotateRunning reports whether a run is already in flight, so the button
// cannot start a second one. These runs read a lot of code and cost real
// tokens; two of them racing would double that for nothing.
func (s *Store) AnnotateRunning(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps, err := s.read()
	if err != nil {
		return false
	}
	for _, p := range ps {
		if p.ID == id {
			return p.Annotate != nil && p.Annotate.State == "running"
		}
	}
	return false
}

// AssignedAgent returns the agent a project is assigned to, if any. The label
// run executes as that agent so propose_project_dir_info sees the same grant an
// ordinary analysis would.
func (s *Store) AssignedAgent(id string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps, err := s.read()
	if err != nil {
		return "", err
	}
	for _, p := range ps {
		if p.ID != id {
			continue
		}
		for _, g := range p.Grants {
			if g.ExpiresAt == nil || time.Now().Before(*g.ExpiresAt) {
				return g.Agent, nil
			}
		}
		return "", fmt.Errorf("项目尚未分配给任何 Agent，请先分配后再生成标注")
	}
	return "", ErrNotFound
}

// PendingDrafts counts the drafts awaiting review.
func (s *Store) PendingDrafts(id string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps, err := s.read()
	if err != nil {
		return 0
	}
	for _, p := range s.hydrate(ps) {
		if p.ID == id {
			return len(p.DirectoryDrafts)
		}
	}
	return 0
}
