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

// TestSkillVarsMasked verifies skill variables persist (in config) and that the
// API never echoes their values — only the key names.
func TestSkillVarsMasked(t *testing.T) {
	s := newEmptyServer(t)
	// Create the skill first so detail works.
	if w := do(t, s, "POST", "/api/skills", `{"name":"deploy","description":"部署","body":"b","enabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("create skill: %d", w.Code)
	}
	w := do(t, s, "POST", "/api/skills/deploy/vars", `{"vars":{"API_TOKEN":"super-secret-xyz"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("set vars status = %d: %s", w.Code, w.Body.String())
	}
	if got := s.engine().Config().SkillVars["deploy"]["API_TOKEN"]; got != "super-secret-xyz" {
		t.Fatalf("var not persisted: %q", got)
	}
	// Detail exposes the key, never the value.
	w = do(t, s, "GET", "/api/skills/deploy", "")
	body := w.Body.String()
	if strings.Contains(body, "super-secret-xyz") {
		t.Fatalf("var value leaked: %s", body)
	}
	if !strings.Contains(body, `"var_keys":["API_TOKEN"]`) {
		t.Fatalf("var key not surfaced: %s", body)
	}
	// Delete the var.
	if w := do(t, s, "DELETE", "/api/skills/deploy/vars/API_TOKEN", ""); w.Code != http.StatusOK {
		t.Fatalf("delete var: %d", w.Code)
	}
	if _, ok := s.engine().Config().SkillVars["deploy"]; ok {
		t.Fatalf("skill vars not cleaned up after last delete")
	}
}

func TestAllowScriptsToggle(t *testing.T) {
	s := newEmptyServer(t)
	if s.engine().Config().Skills.AllowScripts {
		t.Fatal("scripts should be off by default")
	}
	w := do(t, s, "POST", "/api/skills/allow-scripts", `{"enabled":true}`)
	if w.Code != http.StatusOK || !s.engine().Config().Skills.AllowScripts {
		t.Fatalf("enable failed: code=%d on=%v", w.Code, s.engine().Config().Skills.AllowScripts)
	}
	w = do(t, s, "GET", "/api/skills", "")
	if !strings.Contains(w.Body.String(), `"allow_scripts":true`) {
		t.Fatalf("list missing allow_scripts: %s", w.Body.String())
	}
}

func TestSkillRejectsBadName(t *testing.T) {
	s := newEmptyServer(t)
	w := do(t, s, "POST", "/api/skills", `{"name":"bad name","description":"d","body":"b","enabled":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for bad name", w.Code)
	}
}
