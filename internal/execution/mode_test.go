package execution

import (
	"path/filepath"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

func TestSessionAskingForEveryCommandTurnsReadsIntoApprovals(t *testing.T) {
	s := approvalStore(t)
	c := approvalConfig()
	read := Request{Command: "kubectl get pods", Purpose: "看看", Profile: "read"}
	if _, err := s.Create(WithOrigin(t.Context(), "c", "p"), c, "ops", "s", "round", "call", read); err == nil {
		t.Fatal("a direct read needed no approval yet one was created")
	}
	if strict, available := s.SessionMode(t.Context(), "s"); strict || !available {
		t.Fatal(strict, available)
	}
	if err := s.SetStrict(t.Context(), "s", true, "admin"); err != nil {
		t.Fatal(err)
	}
	a, err := s.Create(WithOrigin(t.Context(), "c", "p"), c, "ops", "s", "round", "call", read)
	if err != nil || a.Reason != StrictReason {
		t.Fatalf("strict session did not ask: %+v %v", a, err)
	}
	if s.Strict(t.Context(), "other") {
		t.Fatal("strict leaked to another session")
	}
	if err := s.SetStrict(t.Context(), "s", false, "admin"); err != nil {
		t.Fatal(err)
	}
	if s.Strict(t.Context(), "s") {
		t.Fatal("switch back ignored")
	}
}

func TestUnmigratedStoreHidesTheSwitch(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := Approvals{DB: db}
	if strict, available := s.SessionMode(t.Context(), "s"); strict || available {
		t.Fatal(strict, available)
	}
	if err := s.SetStrict(t.Context(), "s", true, "admin"); err != ErrModeUnavailable {
		t.Fatalf("want ErrModeUnavailable, got %v", err)
	}
}
