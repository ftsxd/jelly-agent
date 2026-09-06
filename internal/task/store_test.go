package task

import (
	"path/filepath"
	"testing"

	_ "github.com/glebarez/go-sqlite"
)

// A task id is derived from the run that opened it, so it stays readable and
// stays joinable — every other table is keyed by (session, invocation), and an
// id that is those two spelled out can be traced by eye through any of them.
func TestIDRoundTrips(t *testing.T) {
	for _, tc := range []struct{ session, inv string }{
		{"web-1788515416639163000-1", "e-76faf8b2-1234"},
		{"schedule-nightly", "inv-1"},
	} {
		s, i := Split(ID(tc.session, tc.inv))
		if s != tc.session || i != tc.inv {
			t.Errorf("round trip of (%q,%q) gave (%q,%q)", tc.session, tc.inv, s, i)
		}
	}
	// A malformed id must not silently become something else.
	s, i := Split("no-slash")
	if s != "no-slash" || i != "" {
		t.Errorf("Split(%q) = (%q,%q)", "no-slash", s, i)
	}
}

func TestLinkingRunsToATask(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")

	// Nothing linked: every run is its own task, which is the common case and
	// the only case for anything written before this table existed.
	got, err := OfSession(db, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("an empty store reported links: %v", got)
	}

	if err := Link(db, "web-1/inv-1", "web-1", "inv-2"); err != nil {
		t.Fatal(err)
	}
	got, err = OfSession(db, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if got["inv-2"] != "web-1/inv-1" {
		t.Errorf("links = %v", got)
	}

	// Idempotent: a retry or a reconnect must not create a second membership.
	if err := Link(db, "web-1/inv-1", "web-1", "inv-2"); err != nil {
		t.Fatal(err)
	}
	got, _ = OfSession(db, "web-1")
	if len(got) != 1 {
		t.Errorf("re-linking created %d rows", len(got))
	}

	// Another session's links are not this one's.
	if other, _ := OfSession(db, "web-2"); len(other) != 0 {
		t.Errorf("cross-session leak: %v", other)
	}
}

func TestLinkRejectsIncompleteIdentities(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	for _, tc := range [][3]string{
		{"", "s", "i"}, {"t", "", "i"}, {"t", "s", ""},
	} {
		if err := Link(db, tc[0], tc[1], tc[2]); err == nil {
			t.Errorf("Link(%q,%q,%q) was accepted", tc[0], tc[1], tc[2])
		}
	}
}

// The override exists so a test or a second deployment can point elsewhere.
// A store that resolves its own path ignores it — which is how a test came to
// write links into the developer's real database.
func TestTheDatabasePathIsHonoured(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a.db")
	b := filepath.Join(t.TempDir(), "b.db")
	if err := Link(a, "web-1/inv-1", "web-1", "inv-2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := OfSession(b, "web-1"); len(got) != 0 {
		t.Errorf("a link written to one database was visible in another: %v", got)
	}
	if got, _ := OfSession(a, "web-1"); len(got) != 1 {
		t.Errorf("the link is not in the database it was written to")
	}
}
