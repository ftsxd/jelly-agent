package codeproject

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func testProject() Project {
	return Project{ID: "service", Name: "Service", URL: "https://git.example.com/team/service.git", Branch: "main"}
}
func seedSnapshot(t *testing.T, s *Store) string {
	t.Helper()
	dir := filepath.Join(s.dir, "snapshot-test")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.read()
	ps[0].Snapshot = "snapshot-test"
	now := time.Now()
	ps[0].SyncedAt = &now
	ps[0].Revision = "old"
	if err := s.write(ps); err != nil {
		t.Fatal(err)
	}
	return dir
}
func TestGrantsAreLiveDefaultDenyAndExpire(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	seedSnapshot(t, s)
	read := func(agent string) error {
		return s.WithRead(agent, p.ID, func(dir string, _ Project) error { _, err := os.ReadFile(filepath.Join(dir, "main.go")); return err })
	}
	if !errors.Is(read("analyst"), ErrDenied) {
		t.Fatal("default must deny")
	}
	future := time.Now().Add(time.Hour)
	if err := s.SetGrants(p.ID, []Grant{{Agent: "analyst", ExpiresAt: &future}}); err != nil {
		t.Fatal(err)
	}
	if err := read("analyst"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(read("child"), ErrDenied) {
		t.Fatal("child inherited access")
	}
	reopened := Open(s.Dir())
	if reopened != s {
		t.Fatal("reload lost shared locks")
	}
	visible, err := reopened.Visible("analyst")
	if err != nil || len(visible) != 1 || visible[0].URL != "" || visible[0].Snapshot != "" {
		t.Fatalf("visible metadata: %+v %v", visible, err)
	}
	// Ordinary project edits must preserve independently updated grants.
	p.Name = "renamed"
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := read("analyst"); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Second)
	if err := s.SetGrants(p.ID, []Grant{{Agent: "analyst", ExpiresAt: &past}}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(read("analyst"), ErrDenied) {
		t.Fatal("expired grant accepted")
	}
	if err := s.SetGrants(p.ID, []Grant{{Agent: "analyst"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrants(p.ID, nil); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(read("analyst"), ErrDenied) {
		t.Fatal("revoked grant accepted")
	}
}
func TestUntrustedConfigCannotPublishSnapshots(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	p.Snapshot = "snapshot-injected"
	p.Revision = "fake"
	now := time.Now()
	p.SyncedAt = &now
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.List()
	if ps[0].Snapshot != "" || ps[0].Revision != "" || ps[0].SyncedAt != nil {
		t.Fatal("accepted client snapshot")
	}
	dir := seedSnapshot(t, s)
	p.Branch = "develop"
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	ps, _ = s.List()
	if ps[0].Snapshot != "" {
		t.Fatal("branch change kept old source")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale snapshot not removed")
	}
}
func TestValidateRejectsUnsafeInputs(t *testing.T) {
	for _, u := range []string{"file:///etc", "ssh://host/repo", "http://host/repo", "https://user:secret@host/repo", "https://host/repo?token=x", "--upload-pack=bad", "https://host/repo\nheader"} {
		p := testProject()
		p.URL = u
		if Validate(p) == nil {
			t.Fatalf("accepted URL %q", u)
		}
	}
	for _, branch := range []string{"-config=x", "a..b", "a\nb", "x@{y}", "x y", "/"} {
		p := testProject()
		p.Branch = branch
		if Validate(p) == nil {
			t.Fatalf("accepted branch %q", branch)
		}
	}
	p := testProject()
	p.ID = "../escape"
	if Validate(p) == nil {
		t.Fatal("accepted traversal ID")
	}
}
func TestSyncPublicationFailureAndMetadataIsolation(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	p.TokenEnv = "PROJECT_TEST_TOKEN"
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROJECT_TEST_TOKEN", "do-not-leak")
	bin := t.TempDir()
	failure := filepath.Join(bin, "fail")
	script := fmt.Sprintf(`#!/bin/sh
if [ -f '%s' ]; then echo do-not-leak >&2; exit 1; fi
mode=
for arg do
  if [ "$arg" = clone ]; then mode=clone; fi
  if [ "$arg" = rev-parse ]; then mode=revision; fi
  last="$arg"
done
if [ "$mode" = clone ]; then
  /bin/mkdir -p "$last/.git"
  echo private-config > "$last/.git/config"
  echo 'package main' > "$last/main.go"
  /bin/ln -s /etc/passwd "$last/escape"
else
  echo 0123456789abcdef
fi
`, failure)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := s.Sync(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.List()
	old := ps[0]
	if old.Revision != "0123456789abcdef" || old.SyncedAt == nil || old.Syncing {
		t.Fatalf("bad sync result %+v", old)
	}
	for _, file := range []string{".git", "escape"} {
		if _, err := os.Lstat(filepath.Join(s.dir, old.Snapshot, file)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("snapshot exposes %s", file)
		}
	}
	if err := os.WriteFile(failure, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(context.Background(), p.ID); err == nil {
		t.Fatal("failed clone succeeded")
	}
	ps, _ = s.List()
	if ps[0].Snapshot != old.Snapshot || ps[0].LastError == "" || strings.Contains(ps[0].LastError, "do-not-leak") {
		t.Fatalf("failure lost snapshot or exposed token %+v", ps[0])
	}
	if err := s.Delete(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, old.Snapshot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("delete retained snapshot")
	}
}
func TestCanRevokeDuringSync(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	s.busy[p.ID] = "sync-test"
	if !errors.Is(s.Save(p), ErrBusy) || !errors.Is(s.Delete(p.ID), ErrBusy) || !errors.Is(s.Sync(context.Background(), p.ID), ErrBusy) {
		t.Fatal("concurrent mutation allowed")
	}
	if err := s.SetGrants(p.ID, nil); err != nil {
		t.Fatal("revoke blocked during sync", err)
	}
}

func TestRevokeAgentPreservesOtherGrants(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	p.Grants = []Grant{{Agent: "deleted"}, {Agent: "remaining"}}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAgent("deleted"); err != nil {
		t.Fatal(err)
	}
	ps, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps[0].Grants) != 1 || ps[0].Grants[0].Agent != "remaining" {
		t.Fatalf("wrong grants after deletion: %+v", ps[0].Grants)
	}
}

func TestDirectoryConfigIsValidatedAndNormalized(t *testing.T) {
	s := Open(t.TempDir())
	base := testProject()
	for name, mutate := range map[string]func(*Project){
		"absolute main":   func(p *Project) { p.RootPath = "/etc" },
		"traversal main":  func(p *Project) { p.RootPath = "../secrets" },
		"traversal ref":   func(p *Project) { p.ReferencePaths = []string{"common/../../etc"} },
		"backslash ref":   func(p *Project) { p.ReferencePaths = []string{`common\win`} },
		"duplicate ref":   func(p *Project) { p.ReferencePaths = []string{"common", "common"} },
		"ref equals main": func(p *Project) { p.RootPath = "common"; p.ReferencePaths = []string{"common"} },
		"out-of-scope meta": func(p *Project) {
			p.RootPath = "services/order"
			p.DirectoryMeta = map[string]DirectoryInfo{"services/other": {Name: "别人"}}
		},
		"overlong meta name": func(p *Project) { p.DirectoryMeta = map[string]DirectoryInfo{".": {Name: strings.Repeat("x", 201)}} },
	} {
		p := base
		mutate(&p)
		if err := s.Save(p); err == nil {
			t.Fatalf("%s: accepted invalid directory configuration", name)
		}
	}

	// Trailing slashes and redundant segments normalize, and metadata keys
	// normalize the same way so they still match the directory they describe.
	p := base
	p.RootPath = "services/./order/"
	p.ReferencePaths = []string{"common/"}
	p.DirectoryMeta = map[string]DirectoryInfo{"services/order/": {Name: "订单服务", Description: "下单与状态流转。"}}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	ps, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if ps[0].RootPath != "services/order" || ps[0].ReferencePaths[0] != "common" {
		t.Fatalf("paths not normalized: %+v", ps[0])
	}
	dirs := ps[0].Directories()
	if len(dirs) != 2 || dirs[0].Role != "main" || dirs[0].Name != "订单服务" || dirs[1].Role != "reference" {
		t.Fatalf("directories not resolved: %+v", dirs)
	}
}

func TestLegacyProjectAnalysesWholeRepo(t *testing.T) {
	p := Project{ID: "legacy"} // saved before root_path existed
	if p.Main() != "." || len(p.Scope()) != 1 || p.Scope()[0] != "." {
		t.Fatalf("legacy project must default to the whole repository: %v", p.Scope())
	}
	if p.Directories()[0].Path != "." {
		t.Fatal("legacy project lost its directory listing")
	}
}

// Editing directories must not throw away the snapshot: the snapshot holds the
// whole repository, so a directory edit is metadata, not a reason to re-pull.
// Changing the repository or branch still must.
func TestDirectoryEditKeepsSnapshotButRepoChangeDoesNot(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	dir := seedSnapshot(t, s)
	if err := os.MkdirAll(filepath.Join(dir, "services", "order"), 0700); err != nil {
		t.Fatal(err)
	}

	p.RootPath = "services/order"
	p.DirectoryMeta = map[string]DirectoryInfo{"services/order": {Name: "订单服务"}}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.List()
	if ps[0].Snapshot == "" || ps[0].Revision != "old" {
		t.Fatal("directory edit discarded the snapshot")
	}
	if len(ps[0].DirIssues) != 0 {
		t.Fatalf("existing directory reported as missing: %v", ps[0].DirIssues)
	}

	// A directory that is not in the snapshot is reported, not silently empty.
	p.RootPath = "services/gone"
	p.DirectoryMeta = nil
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	ps, _ = s.List()
	if len(ps[0].DirIssues) != 1 || !strings.Contains(ps[0].DirIssues[0], "services/gone") {
		t.Fatalf("missing directory not reported: %v", ps[0].DirIssues)
	}

	p.URL = "https://git.example.com/team/other.git"
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	ps, _ = s.List()
	if ps[0].Snapshot != "" || ps[0].SyncedAt != nil {
		t.Fatal("repository change kept the old snapshot")
	}
}

func TestVisibleCarriesDirectoriesButNotSecrets(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	p.TokenEnv = "GIT_TOKEN"
	p.RootPath = "services/order"
	p.ReferencePaths = []string{"common"}
	p.DirectoryMeta = map[string]DirectoryInfo{"common": {Description: "公共组件"}}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	seedSnapshot(t, s)
	if err := s.SetGrants(p.ID, []Grant{{Agent: "analyst"}}); err != nil {
		t.Fatal(err)
	}
	vs, err := s.Visible("analyst")
	if err != nil || len(vs) != 1 {
		t.Fatal(err, vs)
	}
	if vs[0].Snapshot != "" || vs[0].TokenEnv != "" || vs[0].URL != "" || len(vs[0].Grants) != 0 {
		t.Fatalf("agent-visible project leaked operator fields: %+v", vs[0])
	}
	if vs[0].Directories()[1].Description != "公共组件" {
		t.Fatalf("directory descriptions missing: %+v", vs[0].Directories())
	}
}

// On a Chinese-language console the input method is on by default and a
// full-width ｘ looks almost exactly like an ASCII x. "Invalid" on its own sends
// people looking at the wrong field, so the message has to name the cause.
func TestFullWidthInputIsRejectedWithAnActionableMessage(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	p.TokenEnv = "ｘｘｘｘ" // full-width
	err := s.Save(p)
	if err == nil {
		t.Fatal("full-width credential variable name accepted")
	}
	if !strings.Contains(err.Error(), "输入法") {
		t.Fatalf("message does not name the likely cause: %v", err)
	}
	// The value is never echoed — someone will eventually paste the token here.
	if strings.Contains(err.Error(), "ｘｘｘｘ") {
		t.Fatalf("error message echoed the field value: %v", err)
	}

	// The half-width spelling is the thing that should just work.
	p.TokenEnv = "xxxx"
	if err := s.Save(p); err != nil {
		t.Fatalf("valid credential variable name rejected: %v", err)
	}

	// A hyphen is a different mistake and must not claim to be an IME problem.
	p.TokenEnv = "MY-TOKEN"
	err = s.Save(p)
	if err == nil || strings.Contains(err.Error(), "输入法") {
		t.Fatalf("hyphen misdiagnosed as an input-method problem: %v", err)
	}
}

// A whole-repo project must be able to label every service under it. The
// earlier rule allowed an annotation only on a configured directory, which for
// root "." meant exactly one label for 130 services.
func TestAnyDirectoryInsideTheScopeCanBeAnnotated(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject() // root defaults to the whole repository
	p.DirectoryMeta = map[string]DirectoryInfo{
		"services/order":        {Name: "订单服务", Description: "下单与状态流转。", Tags: []string{"核心链路", "订单"}},
		"services/silkworm_pay": {Name: "支付服务", Tags: []string{"支付", "核心链路"}},
		"common":                {Name: "公共组件"},
	}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	ps, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if got := ps[0].Annotations(); len(got) != 3 {
		t.Fatalf("catalogue lost entries: %+v", got)
	}
	// Sorted by path, and the configured root keeps its role while the rest are
	// labels rather than entry points.
	ann := ps[0].Annotations()
	if ann[0].Path != "common" || ann[0].Role != "annotated" {
		t.Fatalf("unexpected first entry: %+v", ann[0])
	}
	// Tags are stored in a deterministic order (byte order, not pinyin) so the
	// same set never renders two different ways.
	info, ok := ps[0].Annotation("services/order")
	if !ok || len(info.Tags) != 2 {
		t.Fatalf("tags not stored: %+v", info)
	}
	if !sort.StringsAreSorted(info.Tags) {
		t.Fatalf("tags not in a stable order: %v", info.Tags)
	}
}

// Scope containment still holds: labelling something the project may not read
// would be a second way to name a path.
func TestAnnotationOutsideTheScopeIsRefused(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	p.RootPath = "services/order"
	p.ReferencePaths = []string{"common"}
	p.DirectoryMeta = map[string]DirectoryInfo{"services/silkworm_pay": {Name: "支付服务"}}
	if err := s.Save(p); err == nil {
		t.Fatal("annotated a directory the project cannot read")
	}
	// Inside either configured directory is fine, including nested paths.
	p.DirectoryMeta = map[string]DirectoryInfo{
		"services/order/internal/handler": {Name: "下单入口"},
		"common/redis":                    {Name: "缓存封装"},
	}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
}

// Editing the repository URL must not wipe a catalogue the form never carried.
func TestProjectEditKeepsTheAnnotationCatalogue(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	p.DirectoryMeta = map[string]DirectoryInfo{
		"services/order": {Name: "订单服务"},
		"services/pay":   {Name: "支付服务"},
	}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	// The form only ever sends labels for the configured directories.
	edit := testProject()
	edit.Name = "Renamed"
	edit.DirectoryMeta = map[string]DirectoryInfo{".": {Name: "整仓"}}
	if err := s.Save(edit); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.List()
	if len(ps[0].DirectoryMeta) != 3 {
		t.Fatalf("an unrelated edit dropped the catalogue: %+v", ps[0].DirectoryMeta)
	}
	if ps[0].DirectoryMeta["services/order"].Name != "订单服务" {
		t.Fatal("service labels lost")
	}
	if ps[0].DirectoryMeta["."].Name != "整仓" {
		t.Fatal("the form's own row was not applied")
	}
}

// Agent-written labels are suggestions. Feeding them back to the model as
// established fact would launder a guess into the record.
func TestDraftsStayOutOfTheCatalogueUntilAccepted(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrants(p.ID, []Grant{{Agent: "analyst"}}); err != nil {
		t.Fatal(err)
	}
	seedSnapshot(t, s)

	n, err := s.ProposeAnnotations(p.ID, map[string]DirectoryInfo{
		"services/order": {Name: "订单服务（草稿）"},
		"services/pay":   {Name: "支付服务（草稿）"},
	})
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	ps, _ := s.List()
	if len(ps[0].DirectoryMeta) != 0 {
		t.Fatalf("a draft reached the catalogue: %+v", ps[0].DirectoryMeta)
	}
	// And never reaches the agent.
	vis, _ := s.Visible("analyst")
	if len(vis[0].DirectoryDrafts) != 0 {
		t.Fatal("drafts exposed to the agent")
	}
	if len(vis[0].Annotations()) != 0 {
		t.Fatal("drafts counted as annotations")
	}

	if _, err := s.ResolveDrafts(p.ID, []string{"services/order"}, []string{"services/pay"}, false); err != nil {
		t.Fatal(err)
	}
	ps, _ = s.List()
	if ps[0].DirectoryMeta["services/order"].Name != "订单服务（草稿）" {
		t.Fatalf("accepted draft not applied: %+v", ps[0].DirectoryMeta)
	}
	if len(ps[0].DirectoryDrafts) != 0 {
		t.Fatalf("rejected draft still pending: %+v", ps[0].DirectoryDrafts)
	}
}

func TestSetAnnotationEditsOneRow(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	p.DirectoryMeta = map[string]DirectoryInfo{"services/order": {Name: "订单服务"}, "services/pay": {Name: "支付服务"}}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnnotation(p.ID, "services/order/", DirectoryInfo{Name: "订单中心", Tags: []string{"核心链路"}}); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.List()
	if ps[0].DirectoryMeta["services/order"].Name != "订单中心" {
		t.Fatalf("row not updated: %+v", ps[0].DirectoryMeta)
	}
	if ps[0].DirectoryMeta["services/pay"].Name != "支付服务" {
		t.Fatal("editing one row disturbed another")
	}
	// An empty annotation deletes the row rather than storing a blank one.
	if err := s.SetAnnotation(p.ID, "services/order", DirectoryInfo{}); err != nil {
		t.Fatal(err)
	}
	ps, _ = s.List()
	if _, still := ps[0].DirectoryMeta["services/order"]; still {
		t.Fatal("blank annotation kept as an empty row")
	}
}

// The reason annotations moved off the project: a monorepo normally carries
// several projects, and "services/order is the order service" is a fact about
// the repository. Stored per project, each had to be labelled separately, the
// generated labels disagreed, and editing one never reached the other.
func TestAnnotationsAreSharedByEveryProjectOverTheSameRepository(t *testing.T) {
	s := Open(t.TempDir())
	coupon := testProject()
	coupon.ID, coupon.Name = "p-coupon", "优惠券"
	pay := testProject()
	pay.ID, pay.Name = "p-pay", "支付"
	pay.URL = strings.TrimSuffix(coupon.URL, ".git") // the same repo, spelled differently
	for _, p := range []Project{coupon, pay} {
		if err := s.Save(p); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.SetAnnotation("p-coupon", "services/order", DirectoryInfo{Name: "订单服务", Tags: []string{"核心链路"}}); err != nil {
		t.Fatal(err)
	}
	ps, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.DirectoryMeta["services/order"].Name != "订单服务" {
			t.Fatalf("%s did not see the shared label: %+v", p.ID, p.DirectoryMeta)
		}
	}

	// Editing from the other project reaches the first one too.
	if err := s.SetAnnotation("p-pay", "services/order", DirectoryInfo{Name: "订单中心"}); err != nil {
		t.Fatal(err)
	}
	ps, _ = s.List()
	for _, p := range ps {
		if p.DirectoryMeta["services/order"].Name != "订单中心" {
			t.Fatalf("%s kept a stale label: %+v", p.ID, p.DirectoryMeta)
		}
	}

	// A different repository keeps its own catalogue.
	other := testProject()
	other.ID, other.URL = "p-other", "https://git.example.com/team/other.git"
	if err := s.Save(other); err != nil {
		t.Fatal(err)
	}
	ps, _ = s.List()
	for _, p := range ps {
		if p.ID == "p-other" && len(p.DirectoryMeta) != 0 {
			t.Fatalf("labels leaked across repositories: %+v", p.DirectoryMeta)
		}
	}
}

// Sharing must not become a way to learn the shape of a repository you were
// not given. A project scoped to one service sees only its own directories.
func TestSharedAnnotationsAreStillFilteredByScope(t *testing.T) {
	s := Open(t.TempDir())
	whole := testProject() // root "."
	whole.ID = "p-whole"
	narrow := testProject()
	narrow.ID, narrow.RootPath = "p-coupon", "services/discount_coupon"
	for _, p := range []Project{whole, narrow} {
		if err := s.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	for path, name := range map[string]string{
		"services/discount_coupon": "优惠券服务",
		"services/silkworm_pay":    "支付服务",
	} {
		if err := s.SetAnnotation("p-whole", path, DirectoryInfo{Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	ps, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		switch p.ID {
		case "p-whole":
			if len(p.DirectoryMeta) != 2 {
				t.Fatalf("whole-repo project lost labels: %+v", p.DirectoryMeta)
			}
		case "p-coupon":
			if len(p.DirectoryMeta) != 1 {
				t.Fatalf("scoped project saw outside its range: %+v", p.DirectoryMeta)
			}
			if _, leaked := p.DirectoryMeta["services/silkworm_pay"]; leaked {
				t.Fatal("a scoped project learned the payment service exists")
			}
		}
	}

	// And it cannot write outside its scope either.
	if err := s.SetAnnotation("p-coupon", "services/silkworm_pay", DirectoryInfo{Name: "偷偷标"}); err == nil {
		t.Fatal("a scoped project annotated outside its range")
	}
}

// projects.json is what an operator reads and diffs; the catalogue belongs in
// its own file so a label edit is not a change to the project's configuration.
func TestCatalogueLivesInItsOwnFileNotInProjectsJSON(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	p := testProject()
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnnotation(p.ID, "services/order", DirectoryInfo{Name: "订单服务"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "订单服务") || strings.Contains(string(raw), "directory_metadata") {
		t.Fatalf("catalogue written into projects.json: %s", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, annotationsFile)); err != nil {
		t.Fatalf("catalogue file missing: %v", err)
	}
}

// Catalogues saved on projects by an earlier version must survive the upgrade.
func TestOldPerProjectCataloguesMigrate(t *testing.T) {
	dir := t.TempDir()
	// Write a projects.json in the old shape, before any Store touches it.
	legacy := `[{"id":"service","name":"Service","url":"https://git.example.com/team/service.git","branch":"main","grants":[],
	  "directory_metadata":{"services/order":{"name":"订单服务"}},
	  "directory_drafts":{"services/pay":{"name":"支付服务（草稿）"}}}]`
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}

	s := Open(dir) // migration runs here
	ps, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if ps[0].DirectoryMeta["services/order"].Name != "订单服务" {
		t.Fatalf("annotations lost in migration: %+v", ps[0].DirectoryMeta)
	}
	if ps[0].DirectoryDrafts["services/pay"].Name != "支付服务（草稿）" {
		t.Fatalf("drafts lost in migration: %+v", ps[0].DirectoryDrafts)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "projects.json"))
	if strings.Contains(string(raw), "directory_metadata") {
		t.Fatalf("migration left the old copy behind: %s", raw)
	}
}

func TestRepoKeyTreatsTheSameRepositorySpelledTwoWaysAsOne(t *testing.T) {
	same := []string{
		"https://e.coding.net/realmicro/realmicro/silkworm.git",
		"https://e.coding.net/realmicro/realmicro/silkworm",
		"https://E.Coding.NET/realmicro/realmicro/silkworm.git/",
	}
	want := RepoKey(same[0])
	for _, u := range same[1:] {
		if got := RepoKey(u); got != want {
			t.Errorf("RepoKey(%q) = %q, want %q", u, got, want)
		}
	}
	if RepoKey("https://e.coding.net/realmicro/realmicro/other.git") == want {
		t.Error("different repositories collapsed to one key")
	}
}
