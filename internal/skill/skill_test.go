package skill

import (
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

func TestSaveRejectsBadName(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	for _, bad := range []string{"../escape", "has space", "a/b", ""} {
		if err := st.Save(Skill{Name: bad, Description: "d", Body: "b"}); err == nil {
			t.Fatalf("expected error for name %q", bad)
		}
	}
}
