package codeproject

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secret = "tok-SHOULD-NOT-APPEAR-anywhere"

// projects.json is read, diffed, copied between hosts and backed up. A token in
// it rides along on every one of those, so it must live somewhere else.
func TestTokenNeverEntersProjectsJSON(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	p := testProject()
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetToken(p.ID, secret); err != nil {
		t.Fatal(err)
	}
	// Saving again must not move it, and must not drop it either.
	p.Name = "Renamed"
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatal("token was written into projects.json")
	}
	if s.tokenFor(p.ID) != secret {
		t.Fatal("token lost across an unrelated project edit")
	}
}

func TestSecretsFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	if err := s.Save(testProject()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetToken("service", secret); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, secretsFile))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("secrets file is readable beyond its owner: %04o", mode)
	}
}

// The agent-facing view and the console view are both built from Project. The
// token is not a field on it, so neither can leak it; what they may report is
// that one exists.
func TestOnlyTheExistenceOfATokenIsEverReported(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetToken(p.ID, secret); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrants(p.ID, []Grant{{Agent: "analyst"}}); err != nil {
		t.Fatal(err)
	}

	listed, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if !listed[0].HasToken {
		t.Fatal("console cannot tell that a credential is configured")
	}
	if b, _ := json.Marshal(listed); strings.Contains(string(b), secret) {
		t.Fatal("console payload contains the token")
	}

	visible, err := s.Visible("analyst")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(visible); strings.Contains(string(b), secret) {
		t.Fatal("agent-visible payload contains the token")
	}
	if visible[0].HasToken {
		t.Fatal("agent is told about the credential at all")
	}
}

// A project recreated under a reused id must not inherit the old credential.
func TestDeletingAProjectDropsItsToken(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetToken(p.ID, secret); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(p.ID); err != nil {
		t.Fatal(err)
	}
	if s.tokenFor(p.ID) != "" {
		t.Fatal("token survived the project it belonged to")
	}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if s.HasToken(p.ID) {
		t.Fatal("recreated project inherited the deleted project's credential")
	}
}

func TestClearingATokenRemovesTheFile(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	if err := s.Save(testProject()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetToken("service", secret); err != nil {
		t.Fatal(err)
	}
	if err := s.SetToken("service", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, secretsFile)); !os.IsNotExist(err) {
		t.Fatal("cleared credential left the secrets file behind")
	}
	if s.HasToken("service") {
		t.Fatal("cleared credential still reported as present")
	}
}
