package memory

import (
	"context"
	"path/filepath"
	"testing"
)

// TestPurgeSessions verifies that purging a session's index rows makes its text
// unsearchable — the fix for "deleted session still recalled via load_memory".
func TestPurgeSessions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	s, err := NewSearch(dbPath, 5)
	if err != nil {
		t.Fatalf("NewSearch: %v", err)
	}
	defer s.Close()

	sess := buildSession(t, "s1", [][2]string{{"user", "什么是 kubernetes"}})
	if err := s.AddSessionToMemory(context.Background(), sess); err != nil {
		t.Fatalf("AddSessionToMemory: %v", err)
	}
	if got := search(t, s, "kubernetes"); len(got) == 0 {
		t.Fatal("precondition: expected a hit before purge")
	}

	n, err := PurgeSessions(dbPath, []string{"s1"})
	if err != nil {
		t.Fatalf("PurgeSessions: %v", err)
	}
	if n == 0 {
		t.Error("purged 0 rows, want > 0")
	}
	if got := search(t, s, "kubernetes"); len(got) != 0 {
		t.Errorf("after purge got %d hits, want 0", len(got))
	}
}

// TestPurgeToleratesMissingTable covers the L2-never-enabled case: the purge
// helpers must be no-ops on a database without the memory_fts table.
func TestPurgeToleratesMissingTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	if n, err := PurgeSessions(dbPath, []string{"x"}); err != nil || n != 0 {
		t.Errorf("PurgeSessions on empty db = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := PurgeOrphanIndex(dbPath); err != nil || n != 0 {
		t.Errorf("PurgeOrphanIndex on empty db = (%d, %v), want (0, nil)", n, err)
	}
}
