package tool

import (
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

type projectTestContext struct{ agent.StrictContextMock }

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
	out, err := reader.Run(&projectTestContext{}, args)
	if err != nil || !strings.Contains(out["content"].(string), "package main") {
		t.Fatal(out, err)
	}
	for _, path := range []string{"../secret", outside, "escape"} {
		if _, err := reader.Run(&projectTestContext{}, map[string]any{"project": "orders", "path": path}); err == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
	foreign, _ := ProjectTools(store, "coordinator")
	for _, tool := range foreign {
		if tool.Name() == "read_project_file" {
			if _, err := tool.(projectRunnable).Run(&projectTestContext{}, args); err == nil {
				t.Fatal("coordinator inherited analyst permissions")
			}
		}
	}
	if err := store.SetGrants("orders", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Run(&projectTestContext{}, args); err == nil {
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
	out, err := runnableNamed(t, tools, "list_code_projects").Run(&projectTestContext{}, map[string]any{})
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

	if _, err := read.Run(&projectTestContext{}, map[string]any{"project": "orders", "path": "services/order/main.go"}); err != nil {
		t.Fatal("in-range read failed:", err)
	}
	if _, err := read.Run(&projectTestContext{}, map[string]any{"project": "orders", "path": "services/pay/main.go"}); err == nil {
		t.Fatal("read escaped the configured directory")
	}

	// services/ is a navigation node: it shows the way down, not the siblings.
	out, err := list.Run(&projectTestContext{}, map[string]any{"project": "orders", "path": "services"})
	if err != nil {
		t.Fatal(err)
	}
	if names := entryNames(t, out); len(names) != 1 || names[0] != "order" {
		t.Fatalf("browsing exposed sibling services: %v", names)
	}

	out, err = grep.Run(&projectTestContext{}, map[string]any{"project": "orders", "pattern": "Marker"})
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
	out, err := runnableNamed(t, tools, "grep_project_files").Run(&projectTestContext{}, map[string]any{"project": "legacy", "pattern": "Marker"})
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
	out, err := runnableNamed(t, tools, "list_code_projects").Run(&projectTestContext{}, map[string]any{})
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
	out, err := runnableNamed(t, tools, "list_project_dir").Run(&projectTestContext{}, map[string]any{"project": "silkworm", "path": "services"})
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

	out, err := search.Run(&projectTestContext{}, map[string]any{"project": "silkworm", "query": "订单"})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(out); len(got) != 1 || got[0] != "services/order" {
		t.Fatalf("name lookup failed: %v", got)
	}

	// A tag is how "所有核心链路的服务" gets answered without reading code.
	out, err = search.Run(&projectTestContext{}, map[string]any{"project": "silkworm", "tag": "核心链路"})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(out); len(got) != 2 {
		t.Fatalf("tag lookup failed: %v", got)
	}

	// An empty result must not read as "the code does not exist".
	out, err = search.Run(&projectTestContext{}, map[string]any{"project": "silkworm", "query": "库存"})
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
	out, err := runnableNamed(t, tools, "list_project_tags").Run(&projectTestContext{}, map[string]any{"project": "silkworm"})
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
	if _, err := propose.Run(&projectTestContext{}, map[string]any{
		"project": "silkworm", "directories": []any{map[string]any{"path": "services/pay", "name": "支付服务"}},
	}); err == nil {
		t.Fatal("proposed a label for a directory the project cannot read")
	}
	// A path that does not exist in the snapshot.
	if _, err := propose.Run(&projectTestContext{}, map[string]any{
		"project": "silkworm", "directories": []any{map[string]any{"path": "services/order/nope", "name": "无中生有"}},
	}); err == nil {
		t.Fatal("proposed a label for a directory that does not exist")
	}
	// An unassigned agent cannot propose at all.
	foreign, _ := ProjectTools(store, "coordinator")
	if _, err := runnableNamed(t, foreign, "propose_project_dir_info").Run(&projectTestContext{}, map[string]any{
		"project": "silkworm", "directories": []any{map[string]any{"path": "services/order", "name": "x"}},
	}); err == nil {
		t.Fatal("an unassigned agent wrote a draft")
	}

	// A valid proposal lands as a draft and stays invisible to every read tool.
	if _, err := propose.Run(&projectTestContext{}, map[string]any{
		"project": "silkworm",
		"directories": []any{map[string]any{
			"path": "services/order", "name": "订单服务（草稿）", "tags": []any{"核心链路"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := runnableNamed(t, tools, "search_project_dirs").Run(&projectTestContext{}, map[string]any{"project": "silkworm", "query": "订单"})
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
	out, _ = runnableNamed(t, tools, "search_project_dirs").Run(&projectTestContext{}, map[string]any{"project": "silkworm", "query": "订单"})
	if raw, _ := json.Marshal(out); !strings.Contains(string(raw), "订单服务") {
		t.Fatalf("accepted draft never became an annotation: %s", raw)
	}
}
