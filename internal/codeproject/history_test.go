package codeproject

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedHistory builds the state a sync leaves behind: a bare object cache
// fetched shallowly from a real repository, plus the projects.json that names
// the published revision. Built with real git rather than fixtures because what
// is under test is how git answers, not how we would have liked it to.
func seedHistory(t *testing.T, depth string, p Project) (*Store, map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	source := t.TempDir()
	run(source, "init", "-b", "main")
	revisions := map[string]string{}
	commit := func(key, message string, files map[string]string) {
		t.Helper()
		for name, body := range files {
			full := filepath.Join(source, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		run(source, "add", ".")
		run(source, "commit", "-qm", message)
		revisions[key] = run(source, "rev-parse", "HEAD")
	}
	commit("initial", "初始：订单与支付", map[string]string{
		"services/order/main.go": "package order\n\nconst Version = 1\n",
		"services/pay/main.go":   "package pay\n\nconst Rate = 1\n",
		"secret/keys.txt":        "ROOT_PASSWORD=hunter2\n",
	})
	commit("order", "订单: 增加退款字段", map[string]string{
		"services/order/main.go": "package order\n\nconst Version = 2\n\nvar Refundable = true\n",
	})
	commit("pay", "支付: 调整费率", map[string]string{
		"services/pay/main.go": "package pay\n\nconst Rate = 3\n",
	})
	commit("secret", "轮换密钥", map[string]string{
		"secret/keys.txt": "ROOT_PASSWORD=correct-horse\n",
	})

	dir := t.TempDir()
	cache := filepath.Join(dir, "cache-"+p.ID+".git")
	run(dir, "init", "--bare", cache)
	run(cache, "fetch", "--depth="+depth, "--no-tags", "--", source, "+refs/heads/main:refs/heads/snapshot")
	state, _ := json.Marshal(gitCacheState{URL: p.URL, Branch: p.Branch})
	if err := os.WriteFile(filepath.Join(cache, "codeproject.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p.Revision, p.Snapshot, p.SyncedAt = revisions["secret"], "snapshot-test", &now
	b, _ := json.Marshal([]Project{p})
	if err := os.WriteFile(filepath.Join(dir, "projects.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return Open(dir), revisions
}

func orderProject() Project {
	return Project{
		ID: "orders", Name: "Orders", URL: "https://git.example.com/silkworm.git", Branch: "main",
		RootPath: "services/order", Grants: []Grant{{Agent: "analyst"}},
	}
}

// The configured directories are the range limit for history exactly as they
// are for reads. A diff is file content: a commit that touched a directory
// outside the scope must not arrive as a patch, which is the one way history
// could hand over what list_project_dir refuses to list.
func TestHistoryStaysInsideTheConfiguredDirectories(t *testing.T) {
	store, revisions := seedHistory(t, "10", orderProject())
	ctx := context.Background()

	log, err := store.LogFor(ctx, "analyst", "orders", LogQuery{Limit: 20, WithFiles: true})
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, c := range log.Commits {
		subjects = append(subjects, c.Subject)
		for _, f := range c.Files {
			if !strings.HasPrefix(f.Path, "services/order/") {
				t.Fatalf("超出范围的文件出现在历史里: %+v", f)
			}
		}
	}
	joined := strings.Join(subjects, "|")
	if !strings.Contains(joined, "增加退款字段") || !strings.Contains(joined, "初始") {
		t.Fatalf("范围内的提交没有列出: %v", subjects)
	}
	if strings.Contains(joined, "调整费率") || strings.Contains(joined, "轮换密钥") {
		t.Fatalf("范围外目录的提交被列了出来: %v", subjects)
	}

	// The same rule on the patch path, where the payload is the file itself.
	diff, err := store.DiffFor(ctx, "analyst", "orders", DiffQuery{From: revisions["initial"], Patch: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Patch, "Refundable") {
		t.Fatalf("范围内的改动没有出现在 diff 里: %q", diff.Patch)
	}
	for _, leak := range []string{"hunter2", "correct-horse", "secret/keys.txt", "services/pay"} {
		if strings.Contains(diff.Patch, leak) || strings.Contains(patchPaths(diff.Files), leak) {
			t.Fatalf("diff 泄漏了范围外的内容 %q: %q %+v", leak, diff.Patch, diff.Files)
		}
	}

	// Asking directly about an out-of-scope commit is not a different question.
	show, err := store.ShowFor(ctx, "analyst", "orders", ShowQuery{Revision: revisions["secret"], Patch: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(show.Commit.Files) != 0 || strings.Contains(show.Patch, "correct-horse") {
		t.Fatalf("范围外提交的内容被返回: %+v %q", show.Commit.Files, show.Patch)
	}

	// And naming the path outright is refused rather than silently emptied: an
	// empty result would read as "这个目录没有改动过".
	if _, err := store.LogFor(ctx, "analyst", "orders", LogQuery{Path: "services/pay"}); err == nil ||
		!strings.Contains(err.Error(), "越界") {
		t.Fatalf("范围外路径没有被拒绝: %v", err)
	}
	for _, bad := range []string{"../etc", "/etc/passwd"} {
		if _, err := store.LogFor(ctx, "analyst", "orders", LogQuery{Path: bad}); err == nil {
			t.Fatalf("接受了越界路径 %q", bad)
		}
	}
}

func patchPaths(files []FileChange) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f.Path)
		b.WriteString(" ")
		b.WriteString(f.OldPath)
		b.WriteString(" ")
	}
	return b.String()
}

// A grant is rechecked on every history call, like every other project read —
// a conversation already in flight must not outlive the assignment.
func TestHistoryRechecksGrantsAndRevisionArguments(t *testing.T) {
	store, revisions := seedHistory(t, "10", orderProject())
	ctx := context.Background()

	if _, err := store.LogFor(ctx, "intruder", "orders", LogQuery{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("未授权的 Agent 读到了历史: %v", err)
	}
	if err := store.SetGrants("orders", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogFor(ctx, "analyst", "orders", LogQuery{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("撤销授权后仍能读历史: %v", err)
	}
	if err := store.SetGrants("orders", []Grant{{Agent: "analyst"}}); err != nil {
		t.Fatal(err)
	}

	// The revision is a string from a model reaching a command line. Only an
	// object id, optionally with an ancestor suffix.
	for _, bad := range []string{"--upload-pack=touch /tmp/x", "refs/heads/snapshot", "main", "HEAD; ls", "-v"} {
		if _, err := store.ShowFor(ctx, "analyst", "orders", ShowQuery{Revision: bad}); err == nil {
			t.Fatalf("接受了非法 revision %q", bad)
		}
	}
	for _, good := range []string{"", "HEAD", revisions["order"], revisions["order"][:8], "HEAD~1"} {
		if _, err := store.ShowFor(ctx, "analyst", "orders", ShowQuery{Revision: good}); err != nil {
			t.Fatalf("拒绝了合法 revision %q: %v", good, err)
		}
	}
}

// The depth is the honest limit of this feature, so it has to arrive as a fact
// in the result rather than as a silence the model fills in. "只有 3 个提交"
// and "仓库只改过 3 次" are different claims, and only the first is true.
func TestHistoryReportsItsOwnShallowness(t *testing.T) {
	store, revisions := seedHistory(t, "2", orderProject())
	ctx := context.Background()

	log, err := store.LogFor(ctx, "analyst", "orders", LogQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !log.Shallow || log.Depth != 2 {
		t.Fatalf("没有报告本地历史深度: %+v", log.history)
	}
	if !strings.Contains(log.Note, "同步深度") {
		t.Fatalf("note 没有说明提交数为什么偏少: %q", log.Note)
	}
	if log.Revision != revisions["secret"] {
		t.Fatalf("历史没有锚定在已发布的快照上: %s", log.Revision)
	}

	// A commit older than the fetch depth is not a bad argument, and saying
	// "没有这个提交" would make the model doubt code it can see.
	_, err = store.DiffFor(ctx, "analyst", "orders", DiffQuery{From: revisions["initial"]})
	if err == nil || !strings.Contains(err.Error(), "本地只保留") {
		t.Fatalf("超出深度的版本没有得到可操作的说明: %v", err)
	}
}

// Before the first incremental sync there is a snapshot but no object cache,
// and a cache left over from another repository is not this project's history.
func TestHistoryRequiresThisProjectsOwnCache(t *testing.T) {
	store, _ := seedHistory(t, "5", orderProject())
	ctx := context.Background()
	marker := filepath.Join(store.Dir(), "cache-orders.git", "codeproject.json")

	state, _ := json.Marshal(gitCacheState{URL: "https://git.example.com/another.git", Branch: "main"})
	if err := os.WriteFile(marker, state, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogFor(ctx, "analyst", "orders", LogQuery{}); !errors.Is(err, ErrNoHistory) {
		t.Fatalf("读了属于别的仓库的缓存: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(store.Dir(), "cache-orders.git")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogFor(ctx, "analyst", "orders", LogQuery{}); !errors.Is(err, ErrNoHistory) {
		t.Fatalf("缓存缺失时没有给出可操作的提示: %v", err)
	}
}

// A whole-repository project has no prefixes to pin, and must still work.
func TestHistoryOnWholeRepositoryProject(t *testing.T) {
	p := orderProject()
	p.RootPath = ""
	store, _ := seedHistory(t, "10", p)
	log, err := store.LogFor(context.Background(), "analyst", "orders", LogQuery{Limit: 10, WithFiles: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Commits) != 4 {
		t.Fatalf("整仓项目没有看到全部提交: %d", len(log.Commits))
	}
}

// Patches are model context, not files: past the cap the answer is a truncation
// the model can act on, not a turn spent on one refactor commit.
func TestHistoryTruncatesOversizedPatches(t *testing.T) {
	store, revisions := seedHistory(t, "10", orderProject())
	ctx := context.Background()

	full, err := store.ShowFor(ctx, "analyst", "orders", ShowQuery{Revision: revisions["order"], Patch: true})
	if err != nil {
		t.Fatal(err)
	}
	if full.Truncated || !strings.Contains(full.Patch, "Refundable") {
		t.Fatalf("普通补丁被误判: %+v %q", full.history, full.Patch)
	}

	restore := maxPatchBytes
	maxPatchBytes = 40
	defer func() { maxPatchBytes = restore }()
	cut, err := store.ShowFor(ctx, "analyst", "orders", ShowQuery{Revision: revisions["order"], Patch: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cut.Truncated || len(cut.Patch) > 40 {
		t.Fatalf("补丁没有在上限处截断: %d %+v", len(cut.Patch), cut.history)
	}
	if !strings.Contains(cut.Note, "截断") {
		t.Fatalf("截断没有告诉模型该怎么办: %q", cut.Note)
	}
	// The metadata around a truncated patch still has to be usable.
	if cut.Commit.Subject == "" || len(cut.Commit.Files) == 0 {
		t.Fatalf("截断吞掉了提交本身: %+v", cut.Commit)
	}
}

// Depth is a per-project setting because its cost is a property of the
// repository: fifty commits is a big fetch on a 400 MB monorepo and nothing at
// all on a small service, and both sit behind the same server.
func TestHistoryDepthPrefersTheProjectOverTheDeployment(t *testing.T) {
	deployment := Limits{HistoryDepth: 120}
	for _, c := range []struct {
		name    string
		project int
		limits  Limits
		want    int
	}{
		{"项目自己设了就按项目的", 300, deployment, 300},
		{"项目留空时按部署配置", 0, deployment, 120},
		{"两处都没有就按默认", 0, Limits{}, DefaultHistoryDepth},
		{"项目值超上限时收到上限", maxHistoryDepth + 5000, deployment, maxHistoryDepth},
	} {
		p := orderProject()
		p.HistoryDepth = c.project
		if got := historyDepth(p, c.limits); got != c.want {
			t.Errorf("%s: 得到 %d，应为 %d", c.name, got, c.want)
		}
	}

	// The form is where a bad value must stop, not the git command line.
	p := orderProject()
	for _, bad := range []int{-1, maxHistoryDepth + 1} {
		p.HistoryDepth = bad
		if err := Validate(p); err == nil {
			t.Errorf("接受了非法的历史深度 %d", bad)
		}
	}
	for _, ok := range []int{0, 1, 50, maxHistoryDepth} {
		p.HistoryDepth = ok
		if err := Validate(p); err != nil {
			t.Errorf("拒绝了合法的历史深度 %d: %v", ok, err)
		}
	}
}
