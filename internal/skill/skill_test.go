package skill

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRoundtrip(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in := Skill{Name: "weekly-report", Description: "把本周事项整理成中文周报", Enabled: true, Body: "## 步骤\n1. 收集\n2. 汇总"}
	if err := st.Save(in); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, ok, err := st.Get("weekly-report")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Name != in.Name || got.Description != in.Description || !got.Enabled || got.Body != in.Body {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	list, err := st.List()
	if err != nil || len(list) != 1 || list[0].Name != "weekly-report" {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	if err := st.Delete("weekly-report"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := st.Get("weekly-report"); ok {
		t.Fatal("still present after delete")
	}
	// Deleting a missing skill is not an error.
	if err := st.Delete("weekly-report"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestCatalogOnlyEnabled(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	_ = st.Save(Skill{Name: "on", Description: "启用项", Enabled: true, Body: "x"})
	_ = st.Save(Skill{Name: "off", Description: "停用项", Enabled: false, Body: "y"})

	cat, err := st.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cat, "on：启用项") || !strings.Contains(cat, "use_skill") {
		t.Fatalf("catalog missing enabled skill / hint: %q", cat)
	}
	if strings.Contains(cat, "off") {
		t.Fatalf("catalog leaked disabled skill: %q", cat)
	}
}

func TestCatalogEmptyWhenNone(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	_ = st.Save(Skill{Name: "off", Description: "停用", Enabled: false, Body: "y"})
	cat, err := st.Catalog()
	if err != nil || cat != "" {
		t.Fatalf("catalog = %q err=%v, want empty", cat, err)
	}
}

// makeZip builds an in-memory zip from name→content entries.
func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImportZip(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir)
	zipBytes := makeZip(t, map[string]string{
		// a skill folder with SKILL.md + a bundled resource + a zip-slip attempt
		"my-skill/SKILL.md":      "---\nname: my-skill\ndescription: 测试技能\nenabled: false\n---\n## 步骤\n做事",
		"my-skill/refs/note.txt": "reference",
		"../evil.txt":            "should be skipped",
	})

	sk, err := st.ImportZip(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if sk.Name != "my-skill" || !sk.Enabled { // import normalizes to enabled=true
		t.Fatalf("imported skill = %+v, want name=my-skill enabled=true", sk)
	}

	// Get reads the directory-form skill.
	got, ok, err := st.Get("my-skill")
	if err != nil || !ok || !strings.Contains(got.Body, "做事") {
		t.Fatalf("get after import: ok=%v body=%q err=%v", ok, got.Body, err)
	}
	// Bundled resource extracted; zip-slip entry NOT written outside the dir.
	if _, err := os.Stat(filepath.Join(dir, "my-skill", "refs", "note.txt")); err != nil {
		t.Fatalf("bundled resource missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err == nil {
		t.Fatal("zip-slip escaped the skills dir")
	}
	// It shows up in List and Catalog (enabled).
	cat, _ := st.Catalog()
	if !strings.Contains(cat, "my-skill：测试技能") {
		t.Fatalf("catalog missing imported skill: %q", cat)
	}
}

func TestImportZipNoSkillMd(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	zb := makeZip(t, map[string]string{"readme.txt": "hi"})
	if _, err := st.ImportZip(bytes.NewReader(zb), int64(len(zb))); err == nil {
		t.Fatal("expected error when zip has no SKILL.md")
	}
}

func TestSaveRejectsBadName(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	for _, bad := range []string{"../escape", "has space", "a/b", ""} {
		if err := st.Save(Skill{Name: bad, Description: "d", Body: "b"}); err == nil {
			t.Fatalf("expected error for name %q", bad)
		}
	}
}
