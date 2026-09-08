package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/ops"
)

type declBody struct {
	Dir   string        `json:"dir"`
	File  string        `json:"file"`
	Tools []toolDeclDTO `json:"tools"`
	Kinds []struct {
		Value string `json:"value"`
		Label string `json:"label"`
	} `json:"kinds"`
}

// The layer existed and nobody could reach it: declaring what an MCP tool
// produces meant writing a YAML file AND pointing a config key at it, and the
// key had no default. This is the round trip that removes both.
func TestDeclaringAToolFromTheConsole(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	// Nothing declared yet, and the page still knows where things go and what
	// the choices are.
	w := do(t, s, "GET", "/api/tools/metadata", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var before declBody
	if err := json.Unmarshal(w.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if before.Dir != dir || before.File != consoleFile {
		t.Errorf("dir = %q file = %q", before.Dir, before.File)
	}
	if len(before.Kinds) == 0 {
		t.Error("the console has no vocabulary to offer")
	}
	for _, d := range before.Tools {
		if d.Name == "query_instant" {
			t.Fatal("precondition: query_instant should not be declared yet")
		}
	}

	body := `{"name":"query_instant","server":"n9e-mcp","produces":"metric_series","side_effect":"read_only"}`
	if w := do(t, s, "POST", "/api/tools/metadata", body); w.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", w.Code, w.Body.String())
	}

	// It is on disk, in the file the console owns.
	raw, err := os.ReadFile(filepath.Join(dir, consoleFile))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(raw), "query_instant") || !contains(string(raw), "metric_series") {
		t.Errorf("file = %s", raw)
	}

	// And the registry has it, which is the only thing that makes it real.
	w = do(t, s, "GET", "/api/tools/metadata", "")
	var after declBody
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	var got *toolDeclDTO
	for i := range after.Tools {
		if after.Tools[i].Name == "query_instant" {
			got = &after.Tools[i]
		}
	}
	if got == nil {
		t.Fatalf("the declaration did not reach the registry: %+v", after.Tools)
	}
	if got.Produces != "metric_series" || got.Source != "console" {
		t.Errorf("declaration = %+v", got)
	}
}

func TestConsoleEditsSelectionMetadata(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	body := `{"name":"query_range","server":"n9e-mcp",` +
		`"description":"查询一段时间的监控指标",` +
		`"use_cases":["查询指标曲线","按集群 ID 查询"],` +
		`"examples":["查看集群 CPU 使用率"],` +
		`"anti_examples":["只读取仪表盘配置"],` +
		`"suites":["promql","metrics"]}`
	if w := do(t, s, "POST", "/api/tools/metadata", body); w.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", w.Code, w.Body.String())
	}

	got := declOnDisk(t, dir, "n9e-mcp", "query_range")
	if got.Description != "查询一段时间的监控指标" ||
		!slices.Equal(got.UseCases, []string{"查询指标曲线", "按集群 ID 查询"}) ||
		!slices.Equal(got.Examples, []string{"查看集群 CPU 使用率"}) ||
		!slices.Equal(got.AntiExamples, []string{"只读取仪表盘配置"}) ||
		!slices.Equal(got.Suites, []string{"promql", "metrics"}) {
		t.Fatalf("selection metadata did not round trip: %+v", got)
	}
	listed := declFromAPI(t, s, "query_range")
	if listed.Description != got.Description || !slices.Equal(listed.Suites, got.Suites) {
		t.Errorf("API = %+v, disk = %+v", listed, got)
	}
}

// Clearing a declaration removes it rather than storing a blank one: "not
// declared" and "declared as nothing" would look the same downstream, and only
// the first is true.
func TestClearingADeclarationRemovesIt(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	do(t, s, "POST", "/api/tools/metadata", `{"name":"query_range","server":"n9e-mcp","produces":"metric_series"}`)
	do(t, s, "POST", "/api/tools/metadata", `{"name":"query_range","server":"n9e-mcp","produces":""}`)

	raw, _ := os.ReadFile(filepath.Join(dir, consoleFile))
	if contains(string(raw), "query_range") {
		t.Errorf("the cleared declaration is still on disk: %s", raw)
	}
}

// A value the domain model does not know must be refused at the edge, not
// written to a file that then fails to load for everything else in it.
func TestUnknownValuesAreRefused(t *testing.T) {
	s := newTestServer(t)
	s.engine().Config().Tools.MetadataDir = t.TempDir()
	for _, body := range []string{
		`{"name":"x","produces":"not_a_kind"}`,
		`{"name":"x","side_effect":"whatever"}`,
		`{"name":"","produces":"config"}`,
	} {
		if w := do(t, s, "POST", "/api/tools/metadata", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s → %d, want 400", body, w.Code)
		}
	}
}

// The directory has a default, because a capability reachable only through a
// path nobody knows about is one nobody uses — which is exactly what happened.
func TestTheMetadataDirectoryHasADefault(t *testing.T) {
	cfg := &config.Config{}
	if got := config.ToolMetadataDir(cfg, "/etc/jelly/config.yaml"); got != "/etc/jelly/tools" {
		t.Errorf("beside the config: %q", got)
	}
	// An explicit setting still wins.
	cfg.Tools.MetadataDir = "/custom/place"
	if got := config.ToolMetadataDir(cfg, "/etc/jelly/config.yaml"); got != "/custom/place" {
		t.Errorf("explicit setting ignored: %q", got)
	}
	// Env-only config still resolves somewhere rather than to nothing.
	if got := config.ToolMetadataDir(&config.Config{}, "(env)"); got == "" {
		t.Error("no directory at all for an env-only deployment")
	}
}

// A console declaration must not set a backend.
//
// Backend filters Registry.Available, and a view that asks without an incident
// would then see none of the declared tools — which is the bug that made the
// task view report "执行工具" for tools that were, in fact, declared.
func TestAConsoleDeclarationDoesNotSetABackend(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	do(t, s, "POST", "/api/tools/metadata", `{"name":"list_targets","server":"n9e-mcp","produces":"workload_status"}`)

	reg := s.engine().ToolRegistry()
	m, ok := reg.Lookup("list_targets")
	if !ok || m.Produces != "workload_status" {
		t.Fatalf("lookup = %+v ok=%v", m, ok)
	}
	if m.Backend != "" {
		t.Errorf("backend = %q", m.Backend)
	}

	// The property that matters, and the one whose absence caused the bug:
	// Available filters by backend, so a declaration carrying one is invisible
	// to every view that asks without an incident — which is every view that
	// needed it.
	seen := false
	for _, a := range reg.Available(nil) {
		if a.Name == "list_targets" {
			seen = true
		}
	}
	if !seen {
		t.Error("the declared tool is invisible to Available(nil), so no incident-less view can use it")
	}
}

// A declaration must reach the policy, not only the display.
//
// The gateway is built once per engine and used to hold a frozen snapshot of
// the registry, so declaring a tool from the console changed what the task
// view said about it and not what the gateway would allow — two answers to one
// question, and the quieter one was the one that mattered.
func TestADeclarationReachesTheGatewayNotJustTheDisplay(t *testing.T) {
	s := newTestServer(t)
	s.engine().Config().Tools.MetadataDir = t.TempDir()

	gw := s.engine().ToolGateway()
	if _, ok := gw.Metadata("restart_service"); ok {
		t.Fatal("precondition: restart_service should be unknown")
	}

	body := `{"name":"restart_service","server":"k8s-mcp","produces":"text","side_effect":"mutating_risky"}`
	if w := do(t, s, "POST", "/api/tools/metadata", body); w.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", w.Code, w.Body.String())
	}

	// Same gateway instance — no engine rebuild, because rebuilding would kill
	// every MCP subprocess for the sake of one dropdown.
	m, ok := gw.Metadata("restart_service")
	if !ok {
		t.Fatal("the gateway still cannot see the declaration")
	}
	if m.SideEffect != "mutating_risky" {
		t.Errorf("side effect = %q; the policy would treat it as something else", m.SideEffect)
	}
}

// Two dropdowns, two saves, and neither may lose the other's field.
//
// The page saves on change, so a save carries one field. Sending both — the
// changed one and whatever the page believed the other was — meant each
// request rebuilt the whole entry from a snapshot taken before the other one
// landed, and the second write silently reverted the first. A side-effect
// level lost that way is not cosmetic: that level is what the gateway's
// ceiling policy reads.
func TestSavingOneFieldLeavesTheOtherAlone(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	if w := do(t, s, "POST", "/api/tools/metadata",
		`{"name":"restart_pod","server":"k8s-mcp","side_effect":"mutating_risky"}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	// A second save that says nothing about the side effect must not clear it.
	if w := do(t, s, "POST", "/api/tools/metadata",
		`{"name":"restart_pod","server":"k8s-mcp","produces":"workload_status"}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	got := declOnDisk(t, dir, "k8s-mcp", "restart_pod")
	if string(got.SideEffect) != "mutating_risky" {
		t.Errorf("side_effect = %q；只改 produces 的那次把副作用等级抹掉了", got.SideEffect)
	}
	if string(got.Produces) != "workload_status" {
		t.Errorf("produces = %q", got.Produces)
	}

	// And an explicit empty string still clears — absent and cleared are
	// different requests, which is the whole point of the distinction.
	if w := do(t, s, "POST", "/api/tools/metadata",
		`{"name":"restart_pod","server":"k8s-mcp","side_effect":""}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := declOnDisk(t, dir, "k8s-mcp", "restart_pod"); string(got.SideEffect) != "" {
		t.Errorf("显式清空没有生效: %q", got.SideEffect)
	}
}

// Concurrent saves are serialised, and the file is replaced in one step.
//
// Every save is a read-modify-write of one file. Without a lock two of them
// interleave and one is lost; without an atomic replace a reader — the
// registry's own file watcher, or the next save — can see the truncated file
// and read it as "nothing declared", which would wipe every other tool's
// declaration on the following write.
func TestConcurrentSavesDoNotLoseEachOther(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	const n = 12
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"name":"tool_%d","server":"n9e-mcp","produces":"metric_series"}`, i)
			if w := do(t, s, "POST", "/api/tools/metadata", body); w.Code != http.StatusOK {
				t.Errorf("save %d: status = %d: %s", i, w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()

	decls := readDecls(filepath.Join(dir, consoleFile))
	if len(decls) != n {
		t.Errorf("declarations = %d, want %d — 并发保存互相覆盖了: %+v", len(decls), n, decls)
	}
}

// declOnDisk reads one declaration out of the console's file.
func declOnDisk(t *testing.T, dir, server, name string) ops.ToolMetadata {
	t.Helper()
	for _, m := range readDecls(filepath.Join(dir, consoleFile)) {
		if m.Server == server && m.Name == name {
			return m
		}
	}
	t.Fatalf("%s/%s 不在 %s 里", server, name, consoleFile)
	return ops.ToolMetadata{}
}

// A reader must never catch the file half-written.
//
// The registry watches this directory and re-reads on any change, so it is a
// reader nobody here controls. os.WriteFile truncates and then writes, so a
// re-read landing in between sees an empty or partial file — which parses as
// "nothing declared" and would drop every tool's declaration until the next
// save put them back.
func TestTheDeclarationFileIsNeverSeenHalfWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, consoleFile)
	old := []byte("tools:\n" + strings.Repeat("  - name: a\n", 4000))
	fresh := []byte("tools:\n" + strings.Repeat("  - name: b\n", 4000))
	if err := writeFileAtomic(path, old); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	bad := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(path)
			if err != nil {
				continue // a rename can briefly race an open; that is not a torn read
			}
			if len(b) != len(old) && len(b) != len(fresh) {
				select {
				case bad <- len(b):
				default:
				}
				return
			}
		}
	}()

	for range 200 {
		if err := writeFileAtomic(path, fresh); err != nil {
			t.Fatal(err)
		}
		if err := writeFileAtomic(path, old); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	<-done
	select {
	case n := <-bad:
		t.Errorf("读到了 %d 字节的半成品文件；注册表会把它当成「什么都没声明」", n)
	default:
	}
}

// The page may only claim what is true.
//
// "已声明" used to mean "the console's file contains this key", which says
// nothing about which file the registry actually took the answer from. With
// the console's file loaded first that is normally the same thing — but the
// page has to be able to tell the operator when it is not, rather than showing
// a declaration that the gateway is ignoring.
func TestADeclarationThatDidNotTakeEffectSaysSo(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	// A hand-written file that sorts before console.yaml — the arrangement
	// that used to win on filename alone.
	hand := "tools:\n  - name: query_range\n    server: n9e-mcp\n    produces: log_excerpt\n"
	if err := os.WriteFile(filepath.Join(dir, "a-hand-written.yaml"), []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := do(t, s, "POST", "/api/tools/metadata",
		`{"name":"query_range","server":"n9e-mcp","produces":"metric_series"}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	got := declFromAPI(t, s, "query_range")
	if got.Produces != "metric_series" {
		t.Errorf("生效的是 %q；控制台写的那份应当优先", got.Produces)
	}
	if got.Shadowed {
		t.Error("控制台的声明明明生效了，却报成未生效")
	}

	// And when it genuinely loses — here, to a built-in, which is loaded
	// before any file — the page says so instead of claiming success.
	if w := do(t, s, "POST", "/api/tools/metadata",
		`{"name":"read_result","produces":"topology"}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	shadowed := declFromAPI(t, s, "read_result")
	if shadowed.Produces == "topology" {
		t.Skip("built-in metadata no longer wins; the shadowing case needs another fixture")
	}
	if !shadowed.Shadowed {
		t.Errorf("声明没有生效（实际是 %q），页面却显示已声明", shadowed.Produces)
	}
}

func declFromAPI(t *testing.T, s *Server, name string) toolDeclDTO {
	t.Helper()
	w := do(t, s, "GET", "/api/tools/metadata", "")
	var body declBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, d := range body.Tools {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("%s 不在列表里", name)
	return toolDeclDTO{}
}

// A declaration is a patch, and a patch working alongside a file is not a
// patch being overridden.
//
// The console writes the field somebody set and says nothing about the other.
// Comparing both against the registry meant a tool declared only for what it
// produces read as 未生效 the moment a hand-written file supplied its side
// effect — the two cooperating exactly as intended, reported as a conflict.
func TestAPatchThatTookEffectIsNotReportedAsShadowed(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	hand := "tools:\n  - name: query_range\n    server: n9e-mcp\n" +
		"    description: 查询一段时间的指标曲线\n    side_effect: read_only\n"
	if err := os.WriteFile(filepath.Join(dir, "n9e.yaml"), []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}
	// Only produces — the side effect keeps coming from the file.
	if w := do(t, s, "POST", "/api/tools/metadata",
		`{"name":"query_range","server":"n9e-mcp","produces":"metric_series"}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	got := declFromAPI(t, s, "query_range")
	if got.Produces != "metric_series" {
		t.Errorf("produces = %q；控制台写的那份没有生效", got.Produces)
	}
	if got.Effect != "read_only" {
		t.Errorf("side_effect = %q；文件里那份被抹掉了", got.Effect)
	}
	if got.Shadowed {
		t.Error("补丁和文件正好各管一半，却被报成未生效")
	}
}

// The save's answer has to be the same answer the list would give.
//
// The page applies it straight into the table — that is what keeps it from
// re-reading and racing the next save — so a response that reports only what
// the console wrote showed an inherited field as 未声明 until something else
// forced a reload. The declaration is a patch; its response has to be the
// merged result, not the patch.
func TestTheSaveAnswersWithWhatTookEffect(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	hand := "tools:\n  - name: query_range\n    server: n9e-mcp\n" +
		"    description: 查询一段时间的指标曲线\n    side_effect: read_only\n"
	if err := os.WriteFile(filepath.Join(dir, "n9e.yaml"), []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}

	w := do(t, s, "POST", "/api/tools/metadata",
		`{"name":"query_range","server":"n9e-mcp","produces":"metric_series"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var saved struct {
		Tool toolDeclDTO `json:"tool"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Tool.Produces != "metric_series" {
		t.Errorf("produces = %q", saved.Tool.Produces)
	}
	if saved.Tool.Effect != "read_only" {
		t.Errorf("side_effect = %q；文件里继承来的那个被答成了未声明", saved.Tool.Effect)
	}
	if saved.Tool.Shadowed {
		t.Error("保存成功却答成未生效")
	}

	// And it agrees with the list, which is the property that matters: the
	// page shows one of them and then the other.
	listed := declFromAPI(t, s, "query_range")
	if !reflect.DeepEqual(listed, saved.Tool) {
		t.Errorf("保存返回 %+v，列表返回 %+v——同一个工具两种说法", saved.Tool, listed)
	}
}

// The editor's boxes hold what the console file overrides. The list has to
// keep that separate from what the registry resolved, or the page has no way
// to fill them without inventing overrides.
//
// This is the bug it prevents: opening the editor on an MCP tool filled the
// description box with the MCP server's own English description, and pressing
// 保存元数据 wrote it into the override file — freezing a value that was
// supposed to follow the server, and marking the tool 已声明 for a field
// nobody had decided anything about.
func TestListSeparatesTheConsoleDeclarationFromWhatApplies(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	// web_search is a builtin: it arrives with its own description, which the
	// console does not declare.
	body := `{"name":"web_search","suites":["research"]}`
	if w := do(t, s, "POST", "/api/tools/metadata", body); w.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", w.Code, w.Body.String())
	}

	got := declFromAPI(t, s, "web_search")
	if got.Declared == nil {
		t.Fatal("the console declared this tool and the list does not say what")
	}
	if !slices.Equal(got.Declared.Suites, []string{"research"}) {
		t.Errorf("declared suites = %v", got.Declared.Suites)
	}
	if got.Description == "" {
		t.Fatal("precondition: web_search should resolve to its builtin description")
	}
	if got.Declared.Description != "" {
		t.Errorf("the console declares no description, but the list reports %q as declared",
			got.Declared.Description)
	}
}

// A tool the console says nothing about must not come back looking declared.
func TestListLeavesUndeclaredToolsWithoutADeclaration(t *testing.T) {
	s := newTestServer(t)
	s.engine().Config().Tools.MetadataDir = t.TempDir()

	if got := declFromAPI(t, s, "web_search"); got.Declared != nil {
		t.Errorf("nothing was declared, list reports %+v", got.Declared)
	}
}

// The save's answer is applied straight into the page, so it has to describe
// the declaration the same way the list does — otherwise the boxes refill
// from a different shape the moment somebody saves.
func TestSaveAnswersWithTheSameDeclarationShapeAsTheList(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Tools.MetadataDir = dir

	w := do(t, s, "POST", "/api/tools/metadata", `{"name":"web_search","suites":["research"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", w.Code, w.Body.String())
	}
	var saved struct {
		Tool toolDeclDTO `json:"tool"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	listed := declFromAPI(t, s, "web_search")
	if saved.Tool.Declared == nil || listed.Declared == nil {
		t.Fatalf("save = %+v, list = %+v", saved.Tool.Declared, listed.Declared)
	}
	if !reflect.DeepEqual(saved.Tool.Declared, listed.Declared) {
		t.Errorf("save = %+v, list = %+v", saved.Tool.Declared, listed.Declared)
	}
}
