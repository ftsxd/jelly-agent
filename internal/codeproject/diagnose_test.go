package codeproject

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// The credential must never reach last_error: that field is persisted into
// projects.json, which gets read, copied and backed up.
func TestDiagnoseNeverLeaksTheCredential(t *testing.T) {
	const token = "7b8dba807b0000000000000000737d1684b5f77"
	const auth = "Q0ktQ0QtT1BTOjdiOGRiYTgwN2I="
	// A worst case: git echoes both the header and the raw token.
	stderr := "fatal: Authorization: Basic " + auth + " rejected\nremote: token " + token + " is expired\n"
	got := diagnose(stderr, token, auth, testProject(), errors.New("exit status 128"))
	if strings.Contains(got, token) || strings.Contains(got, auth) {
		t.Fatalf("diagnosis leaked the credential: %s", got)
	}
	if !strings.Contains(got, "***") {
		t.Fatalf("credential was not redacted, just absent by luck: %s", got)
	}
}

// Each of these used to produce the same unusable sentence.
func TestDiagnoseSeparatesTheCauses(t *testing.T) {
	p := testProject()
	p.Branch = "master"
	for _, c := range []struct{ name, stderr, want string }{
		{"401", "fatal: could not read Username for 'https://e.coding.net': terminal prompts disabled", "401"},
		{"403", "remote: HTTP 403 Forbidden", "403"},
		{"404", "remote: Repository not found.", "404"},
		{"missing branch", "fatal: Remote branch nope not found in upstream origin", "分支"},
		{"bad ref", "fatal: couldn't find remote ref master", "分支"},
		{"dns", "fatal: unable to access: Could not resolve host: e.coding.net", "DNS"},
		{"refused", "fatal: unable to access: Failed to connect to e.coding.net port 443", "网络"},
		{"tls", "fatal: unable to access: SSL certificate problem: self signed certificate", "证书"},
	} {
		got := diagnose(c.stderr, "tok", "auth", p, errors.New("exit status 128"))
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: diagnosis missing %q: %s", c.name, c.want, got)
		}
		// The operator still gets git's own words, which is what makes an
		// unanticipated failure diagnosable at all.
		if !strings.Contains(got, "git:") {
			t.Errorf("%s: git's own message was dropped: %s", c.name, got)
		}
	}
}

func TestDiagnoseNamesAMissingGit(t *testing.T) {
	got := diagnose("", "", "", testProject(), &exec.Error{Name: "git", Err: exec.ErrNotFound})
	if !strings.Contains(got, "git") || !strings.Contains(got, "PATH") {
		t.Fatalf("a missing git should say so plainly: %s", got)
	}
}

// An unauthenticated project hitting a private repo gets told to add a token,
// not to go check its token.
func TestDiagnoseTellsAnUnauthenticatedProjectToAddACredential(t *testing.T) {
	got := diagnose("fatal: could not read Username for 'https://x'", "", "", testProject(), errors.New("exit status 128"))
	if !strings.Contains(got, "没有配置凭据") {
		t.Fatalf("missing credential misreported as a bad one: %s", got)
	}
}

func TestBoundedBufferCapsOutputWithoutFailingTheWrite(t *testing.T) {
	var b boundedBuffer
	b.max = 10
	n, err := b.Write([]byte(strings.Repeat("x", 100)))
	if err != nil || n != 100 {
		t.Fatalf("writer must absorb everything it is given: %d %v", n, err)
	}
	if b.b.Len() != 10 {
		t.Fatalf("buffer kept %d bytes, expected the 10-byte cap", b.b.Len())
	}
}
