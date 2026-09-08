package storage_test

import (
	"bytes"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The point of this package is that dialect knowledge exists once. A copy that
// reappears somewhere else is not a style problem — it is the thing that makes
// "support another database" mean "find all seven of them again", and the one
// that gets missed behaves differently from the rest.
//
// So this test reads the tree. It is unusual, and it is here because nothing
// else can catch a reintroduced copy: each one compiles and passes on its own.
func TestDialectKnowledgeLivesOnlyInThisPackage(t *testing.T) {
	banned := []struct{ needle, why string }{
		{`sql.Open(`, "opening a database directly bypasses the shared settings"},
		{"journal_mode", "PRAGMAs belong in the dialect file"},
		{"busy_timeout", "PRAGMAs belong in the dialect file"},
		{"pragma_table_info", "use storage.EnsureColumns; this is SQLite-only syntax"},
		{"SetMaxOpenConns", "the pool policy is a dialect decision"},
		{"glebarez/go-sqlite", "the driver is registered by internal/storage"},
	}

	root := filepath.Join("..", "..", "internal")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		// This package is where it is allowed to live. Tests elsewhere may
		// still open their own scratch databases.
		if strings.Contains(filepath.ToSlash(path), "internal/storage/") ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Comments are not code: the schedule store's opener carries a note
		// about the busy_timeout it used to lack, and prose explaining a
		// dialect decision is exactly what this package wants written down.
		src, err := codeWithoutComments(path)
		if err != nil {
			return err
		}
		for _, b := range banned {
			if strings.Contains(src, b.needle) {
				t.Errorf("%s 出现了 %q —— %s", path, b.needle, b.why)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// codeWithoutComments renders a file's source with every comment dropped.
func codeWithoutComments(path string) (string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0) // 0: do not keep comments
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, f); err != nil {
		return "", err
	}
	return buf.String(), nil
}
