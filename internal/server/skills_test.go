package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestSkillsCRUD drives /api/skills end to end: create, list (no body), fetch
// detail (with body), then delete. Uses newEmptyServer (HOME → temp) so skill
// files land under <tmp>/.jelly-agent/skills.
func TestSkillsCRUD(t *testing.T) {
	s := newEmptyServer(t)

	w := do(t, s, "POST", "/api/skills",
		`{"name":"weekly-report","description":"把本周事项整理成中文周报","body":"## 步骤\n1. 收集","enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}

	// List carries metadata, not the body.
	w = do(t, s, "GET", "/api/skills", "")
	body := w.Body.String()
	if !strings.Contains(body, "weekly-report") || !strings.Contains(body, "把本周事项整理成中文周报") {
		t.Fatalf("list missing skill: %s", body)
	}
	if strings.Contains(body, "## 步骤") {
		t.Fatalf("list should not include body: %s", body)
	}

	// Detail includes the body.
	w = do(t, s, "GET", "/api/skills/weekly-report", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "## 步骤") {
		t.Fatalf("detail missing body: code=%d body=%s", w.Code, w.Body.String())
	}

	// Delete.
	w = do(t, s, "DELETE", "/api/skills/weekly-report", "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d", w.Code)
	}
	w = do(t, s, "GET", "/api/skills/weekly-report", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("after delete status = %d, want 404", w.Code)
	}
}

func TestSkillRejectsBadName(t *testing.T) {
	s := newEmptyServer(t)
	w := do(t, s, "POST", "/api/skills", `{"name":"bad name","description":"d","body":"b","enabled":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for bad name", w.Code)
	}
}
