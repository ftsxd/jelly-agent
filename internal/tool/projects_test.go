package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/codeproject"
	"google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/toolconfirmation"
)

// Ctx is set because the listing derives a deadline for the freshness probe.
// StrictContextMock panics on a nil one, which is the point of it.
type projectTestContext struct {
	agent.StrictContextMock
	session, invocation string
}

func newProjectCtx() *projectTestContext { return newProjectCtxIn("s1", "inv-1") }

// The session and invocation identify one turn of one conversation, which is
// what the stale-snapshot gate holds against.
func newProjectCtxIn(session, invocation string) *projectTestContext {
	return &projectTestContext{
		StrictContextMock: agent.StrictContextMock{Ctx: context.Background()},
		session:           session, invocation: invocation,
	}
}

func (c *projectTestContext) SessionID() string    { return c.session }
func (c *projectTestContext) InvocationID() string { return c.invocation }

func (*projectTestContext) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

type projectRunnable interface {
	Run(adktool.Context, any) (map[string]any, error)
}

func TestProjectToolsBindIdentityAndRecheckLiveGrants(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "snapshot-test")
	if err := os.MkdirAll(snapshot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(snapshot, "escape")); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ps := []codeproject.Project{{ID: "orders", Name: "Orders", URL: "https://git.example.com/orders.git", Branch: "main", Snapshot: "snapshot-test", SyncedAt: &now, Grants: []codeproject.Grant{{Agent: "analyst"}}}}
	b, _ := json.Marshal(ps)
	if err := os.WriteFile(filepath.Join(dir, "projects.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	store := codeproject.Open(dir)
	tools, err := ProjectTools(store, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	var reader projectRunnable
	for _, tool := range tools {
		if tool.Name() == "read_project_file" {
			reader = tool.(projectRunnable)
		}
	}
	args := map[string]any{"project": "orders", "path": "main.go"}
	out, err := reader.Run(newProjectCtx(), args)
	if err != nil || !strings.Contains(out["content"].(string), "package main") {
		t.Fatal(out, err)
	}
	for _, path := range []string{"../secret", outside, "escape"} {
		if _, err := reader.Run(newProjectCtx(), map[string]any{"project": "orders", "path": path}); err == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
	foreign, _ := ProjectTools(store, "coordinator")
	for _, tool := range foreign {
		if tool.Name() == "read_project_file" {
			if _, err := tool.(projectRunnable).Run(newProjectCtx(), args); err == nil {
				t.Fatal("coordinator inherited analyst permissions")
			}
		}
	}
	if err := store.SetGrants("orders", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Run(newProjectCtx(), args); err == nil {
		t.Fatal("already-built tool ignored revocation")
	}
}

// runnableNamed finds one built tool by name.
func runnableNamed(t *testing.T, tools []adktool.Tool, name string) projectRunnable {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() == name {
			return tl.(projectRunnable)
		}
	}
	t.Fatalf("tool %q not built", name)
	return nil
}

// seedProject writes a projects.json plus a snapshot laid out like a small
// monorepo, and returns the store directory.
func seedProject(t *testing.T, p codeproject.Project) *codeproject.Store {
	t.Helper()
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "snapshot-test")
	for _, d := range []string{"services/order", "services/pay", "common"} {
		if err := os.MkdirAll(filepath.Join(snapshot, filepath.FromSlash(d)), 0700); err != nil {
			t.Fatal(err)
		}
		body := "package x\nconst Marker" + filepath.Base(d) + " = 1\n"
		if err := os.WriteFile(filepath.Join(snapshot, filepath.FromSlash(d), "main.go"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	p.Snapshot, p.SyncedAt = "snapshot-test", &now
	b, _ := json.Marshal([]codeproject.Project{p})
	if err := os.WriteFile(filepath.Join(dir, "projects.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return codeproject.Open(dir)
}

// The agent's only way to turn "the order service" into a path is what
// list_code_projects hands it. Without the names and descriptions it has to
// guess, which is the thing the directory configuration exists to prevent.
func TestListCodeProjectsReportsConfiguredDirectories(t *testing.T) {
	store := seedProject(t, codeproject.Project{
		ID: "silkworm-coupon", Name: "Silkworm 优惠券",
		URL: "https://git.example.com/silkworm.git", Branch: "master",
		RootPath: "services/order", ReferencePaths: []string{"common"},
		DirectoryMeta: map[string]codeproject.DirectoryInfo{
			"services/order": {Name: "订单服务", Description: "下单与状态流转。"},
			"common":         {Name: "公共组件"},
		},
		Grants: []codeproject.Grant{{Agent: "analyst"}},
	})
	tools, err := ProjectTools(store, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runnableNamed(t, tools, "list_code_projects").Run(newProjectCtx(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	// Assert on the serialized form: that is what actually reaches the model.
	var got struct {
		Projects []struct {
			ID          string                  `json:"id"`
			URL         string                  `json:"url"`
			Ready       bool                    `json:"ready"`
			Directories []codeproject.Directory `json:"directories"`
		} `json:"projects"`
	}
	raw, _ := json.Marshal(out)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Projects) != 1 || !got.Projects[0].Ready {
		t.Fatalf("project not listed as ready: %s", raw)
	}
	dirs := got.Projects[0].Directories
	if len(dirs) != 2 {
		t.Fatalf("expected the main directory and one reference: %s", raw)
	}
	if dirs[0].Path != "services/order" || dirs[0].Role != "main" || dirs[0].Name != "订单服务" || dirs[0].Description == "" {
		t.Fatalf("main directory not described: %+v", dirs[0])
	}
	if dirs[1].Path != "common" || dirs[1].Role != "reference" || dirs[1].Name != "公共组件" {
		t.Fatalf("reference directory not described: %+v", dirs[1])
	}
	// The repository URL and snapshot name are operator data, not agent data.
	if got.Projects[0].URL != "" || strings.Contains(string(raw), "snapshot-test") {
		t.Fatalf("operator-only fields exposed to the agent: %s", raw)
	}
}

// The range limit has to hold at the tool boundary. A prompt asking the model
// to stay in its directories is not a control.
func TestProjectToolsEnforceTheConfiguredRange(t *testing.T) {
	store := seedProject(t, codeproject.Project{
		ID: "orders", Name: "Orders", URL: "https://git.example.com/silkworm.git", Branch: "master",
		RootPath: "services/order", Grants: []codeproject.Grant{{Agent: "analyst"}},
	})
	tools, err := ProjectTools(store, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	read := runnableNamed(t, tools, "read_project_file")
	list := runnableNamed(t, tools, "list_project_dir")
	grep := runnableNamed(t, tools, "grep_project_files")

	if _, err := read.Run(newProjectCtx(), map[string]any{"project": "orders", "path": "services/order/main.go"}); err != nil {
		t.Fatal("in-range read failed:", err)
	}
	if _, err := read.Run(newProjectCtx(), map[string]any{"project": "orders", "path": "services/pay/main.go"}); err == nil {
		t.Fatal("read escaped the configured directory")
	}

	// services/ is a navigation node: it shows the way down, not the siblings.
	out, err := list.Run(newProjectCtx(), map[string]any{"project": "orders", "path": "services"})
	if err != nil {
		t.Fatal(err)
	}
	if names := entryNames(t, out); len(names) != 1 || names[0] != "order" {
		t.Fatalf("browsing exposed sibling services: %v", names)
	}

	out, err = grep.Run(newProjectCtx(), map[string]any{"project": "orders", "pattern": "Marker"})
	if err != nil {
		t.Fatal(err)
	}
	if files := matchFiles(t, out); len(files) != 1 || files[0] != "services/order/main.go" {
		t.Fatalf("search escaped the configured directory: %v", files)
	}
}

func entryNames(t *testing.T, out map[string]any) []string {
	t.Helper()
	var v struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	raw, _ := json.Marshal(out)
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range v.Entries {
		names = append(names, e.Name)
	}
	return names
}

func matchFiles(t *testing.T, out map[string]any) []string {
	t.Helper()
	var v struct {
		Matches []struct {
			File string `json:"file"`
		} `json:"matches"`
	}
	raw, _ := json.Marshal(out)
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	files := []string{}
	for _, m := range v.Matches {
		files = append(files, m.File)
	}
	return files
}

// Every project created before directory scoping existed keeps reading the
// whole repository.
func TestLegacyProjectStillReadsTheWholeRepository(t *testing.T) {
	store := seedProject(t, codeproject.Project{
		ID: "legacy", Name: "Legacy", URL: "https://git.example.com/silkworm.git", Branch: "master",
		Grants: []codeproject.Grant{{Agent: "analyst"}},
	})
	tools, err := ProjectTools(store, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runnableNamed(t, tools, "grep_project_files").Run(newProjectCtx(), map[string]any{"project": "legacy", "pattern": "Marker"})
	if err != nil {
		t.Fatal(err)
	}
	if files := matchFiles(t, out); len(files) != 3 {
		t.Fatalf("legacy project lost whole-repo access: %v", files)
	}
}

// Nothing syncs the code automatically, so a snapshot pulled a month ago looks
// exactly like one pulled a minute ago unless the listing says otherwise. The
// model would then present stale code as the current state of the repository.
func TestListCodeProjectsReportsSnapshotAge(t *testing.T) {
	store := seedProject(t, codeproject.Project{
		ID: "orders", Name: "Orders", URL: "https://git.example.com/silkworm.git", Branch: "master",
		Grants: []codeproject.Grant{{Agent: "analyst"}},
	})
	// Backdate the snapshot the way a real neglected project would be.
	old := time.Now().Add(-30 * 24 * time.Hour)
	ps, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	ps[0].SyncedAt = &old
	b, _ := json.Marshal(ps)
	if err := os.WriteFile(filepath.Join(store.Dir(), "projects.json"), b, 0600); err != nil {
		t.Fatal(err)
	}

	tools, err := ProjectTools(store, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runnableNamed(t, tools, "list_code_projects").Run(newProjectCtx(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	var got struct {
		Projects []struct {
			SyncedAt string `json:"synced_at"`
			Age      string `json:"snapshot_age"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Projects[0].SyncedAt == "" {
		t.Fatalf("no sync timestamp for the model to judge staleness by: %s", raw)
	}
	if got.Projects[0].Age != "30 天前同步" {
		t.Fatalf("snapshot age not reported in readable form: %q", got.Projects[0].Age)
	}
}

func TestHumanAgeReadsTheWayAPersonWouldSayIt(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "刚刚同步"},
		{5 * time.Minute, "5 分钟前同步"},
		{3 * time.Hour, "3 小时前同步"},
		{50 * time.Hour, "2 天前同步"},
	} {
		if got := humanAge(c.d); got != c.want {
			t.Errorf("humanAge(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func annotatedStore(t *testing.T) *codeproject.Store {
	t.Helper()
	return seedProject(t, codeproject.Project{
		ID: "silkworm", Name: "Silkworm", URL: "https://git.example.com/silkworm.git", Branch: "master",
		DirectoryMeta: map[string]codeproject.DirectoryInfo{
			"services/order": {Name: "订单服务", Description: "下单、查询、取消与状态流转。", Tags: []string{"核心链路", "订单"}},
			"services/pay":   {Name: "支付服务", Description: "支付受理与回调。", Tags: []string{"核心链路", "支付"}},
			"common":         {Name: "公共组件", Tags: []string{"基础设施"}},
		},
		Grants: []codeproject.Grant{{Agent: "analyst"}},
	})
}

// Browsing is where the labels earn their keep: 130 bare directory names tell a
// model nothing about where to look.
func TestListProjectDirCarriesTheLabels(t *testing.T) {
	tools, err := ProjectTools(annotatedStore(t), "analyst")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runnableNamed(t, tools, "list_project_dir").Run(newProjectCtx(), map[string]any{"project": "silkworm", "path": "services"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Entries []struct {
			Name  string   `json:"name"`
			Label string   `json:"label"`
			Note  string   `json:"note"`
			Tags  []string `json:"tags"`
		} `json:"entries"`
	}
	raw, _ := json.Marshal(out)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	for _, e := range got.Entries {
		labels[e.Name] = e.Label
	}
	if labels["order"] != "订单服务" || labels["pay"] != "支付服务" {
		t.Fatalf("labels missing while browsing: %s", raw)
	}
	for _, e := range got.Entries {
		if e.Name == "order" && (e.Note == "" || len(e.Tags) != 2) {
			t.Fatalf("description or tags dropped: %+v", e)
		}
	}
}

func TestSearchProjectDirsFindsByNameAndTag(t *testing.T) {
	tools, err := ProjectTools(annotatedStore(t), "analyst")
	if err != nil {
		t.Fatal(err)
	}
	search := runnableNamed(t, tools, "search_project_dirs")
	paths := func(out map[string]any) []string {
		var v struct {
			Directories []struct {
				Path string `json:"path"`
			} `json:"directories"`
		}
		raw, _ := json.Marshal(out)
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, d := range v.Directories {
			got = append(got, d.Path)
		}
		return got
	}

	out, err := search.Run(newProjectCtx(), map[string]any{"project": "silkworm", "query": "订单"})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(out); len(got) != 1 || got[0] != "services/order" {
		t.Fatalf("name lookup failed: %v", got)
	}

	// A tag is how "所有核心链路的服务" gets answered without reading code.
	out, err = search.Run(newProjectCtx(), map[string]any{"project": "silkworm", "tag": "核心链路"})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(out); len(got) != 2 {
		t.Fatalf("tag lookup failed: %v", got)
	}

	// An empty result must not read as "the code does not exist".
	out, err = search.Run(newProjectCtx(), map[string]any{"project": "silkworm", "query": "库存"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "不代表相关代码不存在") {
		t.Fatalf("empty result gives no caveat: %s", raw)
	}
}

func TestListProjectTagsSummarizesTheCatalogue(t *testing.T) {
	tools, err := ProjectTools(annotatedStore(t), "analyst")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runnableNamed(t, tools, "list_project_tags").Run(newProjectCtx(), map[string]any{"project": "silkworm"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	var got struct {
		Tags []struct {
			Tag         string `json:"tag"`
			Directories int    `json:"directories"`
		} `json:"tags"`
		Annotated int `json:"annotated_directories"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotated != 3 {
		t.Fatalf("annotated count wrong: %s", raw)
	}
	// Most-used first, so the model sees the repository's main axes.
	if got.Tags[0].Tag != "核心链路" || got.Tags[0].Directories != 2 {
		t.Fatalf("tags not ranked by use: %s", raw)
	}
}

// The one write tool in this set. It must not be able to widen what can be
// read, invent directories, or put anything in front of the read tools.
func TestProposeIsDraftOnlyAndScopeBound(t *testing.T) {
	store := seedProject(t, codeproject.Project{
		ID: "silkworm", Name: "Silkworm", URL: "https://git.example.com/silkworm.git", Branch: "master",
		RootPath: "services/order", Grants: []codeproject.Grant{{Agent: "analyst"}},
	})
	tools, err := ProjectTools(store, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	propose := runnableNamed(t, tools, "propose_project_dir_info")

	// Outside the configured scope.
	if _, err := propose.Run(newProjectCtx(), map[string]any{
		"project": "silkworm", "directories": []any{map[string]any{"path": "services/pay", "name": "支付服务"}},
	}); err == nil {
		t.Fatal("proposed a label for a directory the project cannot read")
	}
	// A path that does not exist in the snapshot.
	if _, err := propose.Run(newProjectCtx(), map[string]any{
		"project": "silkworm", "directories": []any{map[string]any{"path": "services/order/nope", "name": "无中生有"}},
	}); err == nil {
		t.Fatal("proposed a label for a directory that does not exist")
	}
	// An unassigned agent cannot propose at all.
	foreign, _ := ProjectTools(store, "coordinator")
	if _, err := runnableNamed(t, foreign, "propose_project_dir_info").Run(newProjectCtx(), map[string]any{
		"project": "silkworm", "directories": []any{map[string]any{"path": "services/order", "name": "x"}},
	}); err == nil {
		t.Fatal("an unassigned agent wrote a draft")
	}

	// A valid proposal lands as a draft and stays invisible to every read tool.
	if _, err := propose.Run(newProjectCtx(), map[string]any{
		"project": "silkworm",
		"directories": []any{map[string]any{
			"path": "services/order", "name": "订单服务（草稿）", "tags": []any{"核心链路"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := runnableNamed(t, tools, "search_project_dirs").Run(newProjectCtx(), map[string]any{"project": "silkworm", "query": "订单"})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(out); strings.Contains(string(raw), "草稿") {
		t.Fatalf("an unaccepted draft reached the read tools: %s", raw)
	}

	// Once a person accepts it, it becomes part of the catalogue.
	if _, err := store.ResolveDrafts("silkworm", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	out, _ = runnableNamed(t, tools, "search_project_dirs").Run(newProjectCtx(), map[string]any{"project": "silkworm", "query": "订单"})
	if raw, _ := json.Marshal(out); !strings.Contains(string(raw), "订单服务") {
		t.Fatalf("accepted draft never became an annotation: %s", raw)
	}
}

// An empty project list is the worst result this tool can return: it is the
// same output whether nothing was ever assigned or the assignment went to a
// different name than the one asking. Inside an agent tree that distinction is
// the whole answer — a grant to the coordinator is not a grant to the sub-agent
// it transfers to — so the result has to name the identity it was checked
// against, and a denial has to do the same.
func TestEmptyProjectListNamesTheIdentityItWasCheckedAgainst(t *testing.T) {
	store := seedProject(t, codeproject.Project{
		ID: "silkworm", Name: "Silkworm", URL: "https://git.example.com/silkworm.git",
		Branch: "master", Grants: []codeproject.Grant{{Agent: "CodeAnalyzer"}},
	})
	tools, err := ProjectTools(store, "OrchestrationAgent")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runnableNamed(t, tools, "list_code_projects").Run(newProjectCtx(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if out["agent"] != "OrchestrationAgent" {
		t.Fatalf("agent = %v, want the identity the grant is matched against", out["agent"])
	}
	hint, _ := out["hint"].(string)
	if !strings.Contains(hint, "OrchestrationAgent") {
		t.Fatalf("hint = %q, want the unassigned identity named", hint)
	}
	// The granted agent gets no hint, and still sees the project.
	granted, err := ProjectTools(store, "CodeAnalyzer")
	if err != nil {
		t.Fatal(err)
	}
	out, err = runnableNamed(t, granted, "list_code_projects").Run(newProjectCtx(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["hint"]; ok {
		t.Fatalf("hint on a non-empty list: %v", out)
	}
	var listed struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	raw, _ := json.Marshal(out)
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Projects) != 1 || listed.Projects[0].ID != "silkworm" {
		t.Fatalf("projects = %s, want the granted project", raw)
	}
	// The denial names the caller too: the merged "不存在/未授权/已过期" message
	// is what makes this unreadable from the transcript alone.
	_, err = runnableNamed(t, tools, "list_project_dir").Run(newProjectCtx(), map[string]any{"project": "silkworm"})
	if err == nil || !strings.Contains(err.Error(), "OrchestrationAgent") {
		t.Fatalf("denial = %v, want the asking identity named", err)
	}
}

// The agent's side of "代码该更新了": it can file the ask, with a reason the
// person will read, and it cannot pull anything itself.
func TestRequestProjectSyncFilesAnAskAndPullsNothing(t *testing.T) {
	store := seedProject(t, codeproject.Project{
		ID: "Silkworm", Name: "Silkworm", URL: "https://git.example.com/silkworm.git",
		Branch: "master", Grants: []codeproject.Grant{{Agent: "CodeAnalyzer"}},
	})
	tools, err := ProjectTools(store, "CodeAnalyzer")
	if err != nil {
		t.Fatal(err)
	}
	req := runnableNamed(t, tools, "request_project_sync")

	if _, err := req.Run(newProjectCtx(), map[string]any{"project": "Silkworm"}); err == nil {
		t.Error("没有理由也能提交：用户看到的就只有这句话")
	}
	out, err := req.Run(newProjectCtx(), map[string]any{"project": "Silkworm", "reason": "要回答最近的改动"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out["status"].(string), "等待用户") {
		t.Errorf("返回没有说清楚这只是个请求：%v", out)
	}
	ps, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	got := ps[0].SyncRequest
	if got == nil || got.Agent != "CodeAnalyzer" || got.Reason != "要回答最近的改动" {
		t.Fatalf("请求没有落到项目上：%+v", got)
	}
	if ps[0].SyncState == codeproject.SyncRunning || ps[0].SyncState == codeproject.SyncQueued {
		t.Errorf("提请求顺手把同步也启动了：%q", ps[0].SyncState)
	}
	// Another agent holding no grant cannot put a card on this project's page.
	foreign, err := ProjectTools(store, "OrchestrationAgent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runnableNamed(t, foreign, "request_project_sync").Run(newProjectCtx(),
		map[string]any{"project": "Silkworm", "reason": "帮个忙"}); err == nil {
		t.Error("未分配的 Agent 也能提请求")
	}
}

// The gate. A conversation that is told its snapshot is behind must ask before
// it analyses — not after 112k tokens of reading the stale tree, with the
// question at the bottom where it can no longer change anything.
func TestStaleSnapshotHoldsTheTurnUntilTheUserIsAsked(t *testing.T) {
	store := seedProject(t, codeproject.Project{
		ID: "Silkworm", Name: "Silkworm", URL: "https://git.example.com/silkworm.git", Branch: "master",
		Revision: "e33c30622411aaaabbbbccccddddeeeeffff0000",
		Grants:   []codeproject.Grant{{Agent: "CodeAnalyzer"}},
	})
	store.SetAutoProbe(true)
	store.SeedRemoteProbeForTest("Silkworm", codeproject.RemoteStatus{
		Local:  "e33c30622411aaaabbbbccccddddeeeeffff0000",
		Remote: "5f2720774f4b1111222233334444555566667777",
		Behind: true,
	})
	tools, err := ProjectTools(store, "CodeAnalyzer")
	if err != nil {
		t.Fatal(err)
	}
	list := runnableNamed(t, tools, "list_code_projects")
	read := runnableNamed(t, tools, "list_project_dir")
	args := map[string]any{"project": "Silkworm", "path": "services/order"}

	// Turn one: the listing reports the staleness and closes the door.
	out, err := list.Run(newProjectCtxIn("chat-1", "inv-1"), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"needs_sync":true`) {
		t.Fatalf("没有报出落后：%s", raw)
	}
	_, err = read.Run(newProjectCtxIn("chat-1", "inv-1"), args)
	if err == nil {
		t.Fatal("被告知快照落后之后，本轮仍然读到了代码")
	}
	if !strings.Contains(err.Error(), "要不要现在同步") {
		t.Fatalf("拒绝没有说清楚该做什么：%v", err)
	}

	// Turn two carries whatever the person answered, so analysis proceeds —
	// including when the answer was "不用同步".
	if _, err := read.Run(newProjectCtxIn("chat-1", "inv-2"), args); err != nil {
		t.Fatalf("用户已经回答过，下一轮却还在拦：%v", err)
	}
	// And a second listing must not re-arm: that would ask the same question
	// on every turn of the conversation.
	if _, err := list.Run(newProjectCtxIn("chat-1", "inv-2"), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Run(newProjectCtxIn("chat-1", "inv-2"), args); err != nil {
		t.Fatalf("同一个会话被反复拦住：%v", err)
	}
	// A different conversation gets its own one question.
	if _, err := list.Run(newProjectCtxIn("chat-2", "inv-9"), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Run(newProjectCtxIn("chat-2", "inv-9"), args); err == nil {
		t.Fatal("另一个会话没有被问")
	}
}

// Confirming a sync must not hand the turn back to the person: they said "更新
// 一下再看", and the answer to "再看" should arrive in the same breath. So the
// tool waits for the pull and reports what happened — including when it failed,
// which is the case a fixture can drive without a git server.
func TestSyncProjectWaitsForThePullToFinish(t *testing.T) {
	store := seedProject(t, codeproject.Project{
		// Nothing listens here, so the pull fails at once instead of leaving a
		// clone running past the test.
		ID: "Silkworm", Name: "Silkworm", URL: "https://127.0.0.1:1/silkworm.git",
		Branch: "master", Grants: []codeproject.Grant{{Agent: "CodeAnalyzer"}},
	})
	tools, err := ProjectTools(store, "CodeAnalyzer")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runnableNamed(t, tools, "sync_project").Run(newProjectCtx(), map[string]any{"project": "Silkworm"})
	if err != nil {
		t.Fatal(err)
	}
	if out["synced"] != false {
		t.Errorf("同步失败却报成已完成：%v", out)
	}
	status, _ := out["status"].(string)
	if !strings.Contains(status, "同步失败") {
		t.Errorf("没有把失败讲清楚：%q", status)
	}
	// It waited: the pull is over by the time the tool returned, which is what
	// lets the model keep analysing in the same turn.
	ps, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if ps[0].SyncState != codeproject.SyncFailed {
		t.Errorf("工具在同步结束前就返回了：state=%q", ps[0].SyncState)
	}
	// And an agent without the grant never gets that far.
	foreign, err := ProjectTools(store, "OrchestrationAgent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runnableNamed(t, foreign, "sync_project").Run(newProjectCtx(), map[string]any{"project": "Silkworm"}); err == nil {
		t.Error("未分配的 Agent 拉起了同步")
	}
}

// History is code analysis, so it ships with the read tools rather than as an
// opt-in extra — and it answers to the same gate. "最近有什么更新" is the exact
// question a stale snapshot answers wrongly and convincingly, so a conversation
// that has just been told the snapshot is behind must not reach for git log to
// answer it either.
func TestHistoryToolsShipWithTheReadToolsAndRespectTheGate(t *testing.T) {
	store := seedProject(t, codeproject.Project{
		ID: "Silkworm", Name: "Silkworm", URL: "https://git.example.com/silkworm.git", Branch: "master",
		Revision: "a1b2c3d4e5f60000111122223333444455556666",
		Grants:   []codeproject.Grant{{Agent: "CodeAnalyzer"}},
	})
	store.SetAutoProbe(true)
	store.SeedRemoteProbeForTest("Silkworm", codeproject.RemoteStatus{
		Local:  "a1b2c3d4e5f60000111122223333444455556666",
		Remote: "9999888877776666555544443333222211110000",
		Behind: true,
	})
	tools, err := ProjectTools(store, "CodeAnalyzer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runnableNamed(t, tools, "list_code_projects").Run(newProjectCtxIn("chat-1", "inv-1"), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string]map[string]any{
		"log_project_commits":    {"project": "Silkworm"},
		"show_project_commit":    {"project": "Silkworm"},
		"diff_project_revisions": {"project": "Silkworm", "from": "a1b2c3d4e5f6"},
	} {
		out, err := runnableNamed(t, tools, name).Run(newProjectCtxIn("chat-1", "inv-1"), args)
		if err == nil {
			t.Fatalf("%s 在快照落后的那一轮仍然作答了：%v", name, out)
		}
		if !strings.Contains(err.Error(), "要不要现在同步") {
			t.Fatalf("%s 的拒绝没有说清楚该做什么：%v", name, err)
		}
	}
	// A diff with nothing to compare against is a mistake worth naming, not a
	// comparison against whatever the empty string resolves to. The schema
	// catches an absent field; an empty one reaches the handler.
	_, err = runnableNamed(t, tools, "diff_project_revisions").Run(newProjectCtxIn("chat-2", "inv-1"), map[string]any{"project": "Silkworm", "from": "  "})
	if err == nil || !strings.Contains(err.Error(), "from 不能为空") {
		t.Fatalf("diff 没有要求给出对比基准：%v", err)
	}
}
