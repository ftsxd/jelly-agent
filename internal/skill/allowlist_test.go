package skill

import "testing"

func ptr(v []string) *[]string { return &v }

// The whole point of the pointer is that "not set" and "set to nothing" are
// different answers. Collapsing them would make a coordinator — an agent that
// should hold no skills — impossible to declare.
func TestAllowlistTriState(t *testing.T) {
	cases := []struct {
		name         string
		in           *[]string
		unrestricted bool
		deniesAll    bool
		permits      map[string]bool
	}{
		{
			name: "未设置 ⇒ 全部", in: nil, unrestricted: true,
			permits: map[string]bool{"a": true, "b": true, "": true},
		},
		{
			name: "列了名字 ⇒ 只有这些", in: ptr([]string{"a", "c"}),
			permits: map[string]bool{"a": true, "c": true, "b": false},
		},
		{
			name: "空列表 ⇒ 一个都没有", in: ptr([]string{}), deniesAll: true,
			permits: map[string]bool{"a": false, "b": false},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := NewAllowlist(c.in)
			if a.Unrestricted() != c.unrestricted {
				t.Errorf("Unrestricted = %v, want %v", a.Unrestricted(), c.unrestricted)
			}
			if a.DeniesAll() != c.deniesAll {
				t.Errorf("DeniesAll = %v, want %v", a.DeniesAll(), c.deniesAll)
			}
			for name, want := range c.permits {
				if got := a.Permits(name); got != want {
					t.Errorf("Permits(%q) = %v, want %v", name, got, want)
				}
			}
		})
	}

	// The zero value is the safe default for a caller with no opinion.
	if !(Allowlist{}).Unrestricted() {
		t.Error("zero Allowlist must be unrestricted")
	}
}

// The catalog is the only thing that tells a model a skill exists, so filtering
// has to happen there — not only at use_skill.
func TestCatalogForFiltersTheCatalog(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	for _, n := range []string{"alpha", "beta"} {
		if err := st.Save(Skill{Name: n, Description: "d-" + n, Enabled: true, Body: "b"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Save(Skill{Name: "off", Description: "d", Enabled: false, Body: "b"}); err != nil {
		t.Fatal(err)
	}

	all, err := st.CatalogFor(Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(all, "alpha") || !contains(all, "beta") {
		t.Fatalf("unrestricted catalog missing a skill: %q", all)
	}
	if contains(all, "off") {
		t.Fatalf("disabled skill leaked into the catalog: %q", all)
	}

	one, err := st.CatalogFor(NewAllowlist(ptr([]string{"alpha"})))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(one, "alpha") || contains(one, "beta") {
		t.Fatalf("narrowed catalog is wrong: %q", one)
	}

	none, err := st.CatalogFor(NewAllowlist(ptr([]string{})))
	if err != nil {
		t.Fatal(err)
	}
	if none != "" {
		t.Fatalf("an agent with no skills must get no catalog block, got %q", none)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
