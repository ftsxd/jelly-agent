package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codebase lays out a small synced project to read.
func codebase(t *testing.T) Roots {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repos")
	mk := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("project-a/main.go", "package main\n\nfunc main() {\n\tserve()\n}\n")
	mk("project-a/internal/handler.go", "package internal\n\nfunc ServeHTTP() {}\nfunc serve() {}\n")
	mk("project-a/.git/config", "[core]\n\tbare = false\n")           // must be skipped by grep
	mk("project-a/node_modules/dep/index.js", "function serve(){}\n") // must be skipped by grep
	mk("project-a/bin/blob", "MZ\x00\x00binary serve payload\x00")    // must be skipped as binary
	mk("project-a/README.md", strings.Repeat("line\n", 50))
	return NewRoots([]string{root})
}

func TestReadFileNumbersLinesAndPaginates(t *testing.T) {
	r := codebase(t)

	res, err := readFile(r, readFileArgs{Path: "project-a/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalLines != 5 || res.FromLine != 1 || res.ToLine != 5 {
		t.Fatalf("line accounting wrong: %+v", res)
	}
	// Numbered lines are what let the model cite a location it can come back to.
	if !strings.HasPrefix(res.Content, "1\tpackage main") {
		t.Fatalf("content not numbered: %q", res.Content)
	}
	if res.Truncated {
		t.Error("a 5-line file should not report truncation")
	}

	// A window in the middle, and the total still reflects the whole file so the
	// model knows there is more.
	res, err = readFile(r, readFileArgs{Path: "project-a/README.md", Offset: 10, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.FromLine != 10 || res.ToLine != 14 || res.TotalLines != 50 || !res.Truncated {
		t.Fatalf("pagination wrong: %+v", res)
	}
	if n := strings.Count(res.Content, "\n"); n != 5 {
		t.Fatalf("limit not honored: %d lines", n)
	}

	// Past the end is a fact to report, not an error to raise.
	res, err = readFile(r, readFileArgs{Path: "project-a/main.go", Offset: 999})
	if err != nil {
		t.Fatalf("reading past the end should not error: %v", err)
	}
	if res.Content != "" || res.Note == "" {
		t.Fatalf("expected an explanatory note, got %+v", res)
	}
}

// A compiled artifact in a repo must not be poured into the model's context.
func TestReadFileRefusesBinaryAndDirectories(t *testing.T) {
	r := codebase(t)

	res, err := readFile(r, readFileArgs{Path: "project-a/bin/blob"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "" || !strings.Contains(res.Note, "二进制") {
		t.Fatalf("binary file was read: %+v", res)
	}

	if _, err := readFile(r, readFileArgs{Path: "project-a/internal"}); err == nil {
		t.Error("reading a directory should point at list_dir instead")
	}
	if _, err := readFile(r, readFileArgs{Path: "../../etc/passwd"}); err == nil {
		t.Error("read_file escaped the root")
	}
}

func TestListDirSortsDirectoriesFirst(t *testing.T) {
	r := codebase(t)
	res, err := listDir(r, listDirArgs{Path: "project-a"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	lastDir := true
	for _, e := range res.Entries {
		names = append(names, e.Name)
		if !e.Dir {
			lastDir = false
		} else if !lastDir {
			t.Fatalf("a directory sorted after a file: %v", names)
		}
	}
	if len(names) == 0 {
		t.Fatal("listing was empty")
	}
	// Sizes are what let a model decide whether reading something is worth it.
	for _, e := range res.Entries {
		if e.Name == "README.md" && e.Bytes == 0 {
			t.Error("file size missing")
		}
	}
	if _, err := listDir(r, listDirArgs{Path: "/etc"}); err == nil {
		t.Error("list_dir escaped the root")
	}
}

func TestGrepFindsCodeAndSkipsNoise(t *testing.T) {
	r := codebase(t)
	res, err := grepFiles(r, grepFilesArgs{Pattern: `serve`})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) == 0 {
		t.Fatal("no matches in a codebase that contains the pattern")
	}
	for _, m := range res.Matches {
		switch {
		case strings.Contains(m.File, ".git/"):
			t.Errorf(".git was searched: %+v", m)
		case strings.Contains(m.File, "node_modules"):
			t.Errorf("node_modules was searched: %+v", m)
		case strings.Contains(m.File, "bin/blob"):
			t.Errorf("a binary file was searched: %+v", m)
		}
		// Paths come back in the form the model can hand to read_file.
		if filepath.IsAbs(m.File) {
			t.Errorf("match path is absolute, not root-relative: %q", m.File)
		}
		if m.Line <= 0 {
			t.Errorf("match without a line number: %+v", m)
		}
	}

	// A glob narrows it; a bad regex is a usage error, not a crash.
	res, err = grepFiles(r, grepFilesArgs{Pattern: `func `, Glob: "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Matches {
		if !strings.HasSuffix(m.File, ".go") {
			t.Errorf("glob not honored: %q", m.File)
		}
	}
	if _, err := grepFiles(r, grepFilesArgs{Pattern: `([`}); err == nil {
		t.Error("an invalid regex should be reported")
	}
	if _, err := grepFiles(r, grepFilesArgs{Pattern: ""}); err == nil {
		t.Error("an empty pattern should be reported")
	}
}

// The match cap is what stops a broad pattern from filling the context window.
func TestGrepRespectsTheMatchCap(t *testing.T) {
	r := codebase(t)
	res, err := grepFiles(r, grepFilesArgs{Pattern: `.`, MaxMatches: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 3 || !res.Truncated {
		t.Fatalf("cap not applied: %d matches truncated=%v", len(res.Matches), res.Truncated)
	}
}

// With no root configured the tools are not built at all.
func TestFileToolsAbsentWithoutRoots(t *testing.T) {
	tools, err := FileTools(Roots{})
	if err != nil || tools != nil {
		t.Fatalf("tools = %v err = %v, want none", tools, err)
	}
	tools, err = FileTools(codebase(t))
	if err != nil || len(tools) != 3 {
		t.Fatalf("expected read_file/list_dir/grep_files, got %d tools err=%v", len(tools), err)
	}
}

// Listing services/ in a project scoped to one service must show the way down
// and nothing else. Handing the model the other services' names would both
// leak the repository's shape and invite reads it is not allowed to make.
func TestListDirShowsNavigationAncestorsWithoutSiblings(t *testing.T) {
	root := scopedRepo(t)
	r := NewScopedRoot(root, []string{"services/order", "common"})

	res, err := listDir(r, listDirArgs{Path: "services"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Name != "order" {
		t.Fatalf("navigation listing exposed siblings: %+v", res.Entries)
	}
	if res.Path != "services" {
		t.Fatalf("navigation listing lost the repo-relative path: %q", res.Path)
	}

	// Inside the scope the listing is the real directory again.
	res, err = listDir(r, listDirArgs{Path: "services/order"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Name != "main.go" {
		t.Fatalf("in-scope listing wrong: %+v", res.Entries)
	}

	if _, err := listDir(r, listDirArgs{Path: "services/pay"}); err == nil {
		t.Fatal("listed a directory outside the configured scope")
	}
}

func TestGrepStaysInsideTheConfiguredDirectories(t *testing.T) {
	root := scopedRepo(t)
	r := NewScopedRoot(root, []string{"services/order", "common"})

	// Empty path means every configured directory, not the whole repository.
	res, err := grepFiles(r, grepFilesArgs{Pattern: "^package "})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 2 {
		t.Fatalf("expected one match per configured directory, got %+v", res.Matches)
	}
	for _, m := range res.Matches {
		if strings.HasPrefix(m.File, "services/pay") || strings.HasPrefix(m.File, "ad/") {
			t.Fatalf("match from outside the scope: %s", m.File)
		}
	}

	// Starting from a navigation ancestor must not widen the search either.
	res, err = grepFiles(r, grepFilesArgs{Pattern: "^package ", Path: "services"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 || res.Matches[0].File != "services/order/main.go" {
		t.Fatalf("search from an ancestor escaped the scope: %+v", res.Matches)
	}

	if _, err := grepFiles(r, grepFilesArgs{Pattern: "^package ", Path: "ad"}); err == nil {
		t.Fatal("searched a directory outside the configured scope")
	}
}

func TestReadFileRefusesOutsideTheConfiguredDirectories(t *testing.T) {
	root := scopedRepo(t)
	r := NewScopedRoot(root, []string{"services/order"})
	if _, err := readFile(r, readFileArgs{Path: "services/order/main.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(r, readFileArgs{Path: "services/pay/main.go"}); err == nil {
		t.Fatal("read a file outside the configured scope")
	}
}

// The root listing is a scoped project's fallback entry point when it has not
// called list_code_projects. It must show the way into the configured
// directories and nothing else.
func TestListDirAtRootShowsOnlyTheWayIntoScope(t *testing.T) {
	root := scopedRepo(t)
	res, err := listDir(NewScopedRoot(root, []string{"services/order", "common"}), listDirArgs{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range res.Entries {
		names = append(names, e.Name)
	}
	if len(names) != 2 || names[0] != "common" || names[1] != "services" {
		t.Fatalf("root listing exposed more than the way into scope: %v", names)
	}
}
