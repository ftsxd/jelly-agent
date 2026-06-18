package server

import (
	"net/http"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/skill"
)

// skillInput is the body for POST /api/skills (create or update).
type skillInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Enabled     bool   `json:"enabled"`
}

// handleListSkills lists the configured skills (metadata only — no body — to
// keep the list light; fetch one with handleSkillDetail for the full text).
func (s *Server) handleListSkills(w http.ResponseWriter, _ *http.Request) {
	store, err := s.engine().Skills()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	all, err := store.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type skillDTO struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Enabled     bool   `json:"enabled"`
	}
	out := make([]skillDTO, 0, len(all))
	for _, sk := range all {
		out = append(out, skillDTO{Name: sk.Name, Description: sk.Description, Enabled: sk.Enabled})
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": out, "dir": store.Dir()})
}

// handleSkillDetail returns one skill including its instruction body.
func (s *Server) handleSkillDetail(w http.ResponseWriter, r *http.Request) {
	store, err := s.engine().Skills()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sk, ok, err := store.Get(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "技能不存在")
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

// handleSaveSkill upserts a skill (writes <name>.md). No engine reload is
// needed: skills are read fresh on the next agent build.
func (s *Server) handleSaveSkill(w http.ResponseWriter, r *http.Request) {
	var in skillInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !skill.ValidName(in.Name) {
		writeErr(w, http.StatusBadRequest, "技能名仅允许字母、数字、下划线、连字符（也用作文件名）")
		return
	}
	if strings.TrimSpace(in.Description) == "" {
		writeErr(w, http.StatusBadRequest, "description 不能为空（它进入技能清单供 Agent 判断）")
		return
	}
	store, err := s.engine().Skills()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sk := skill.Skill{Name: in.Name, Description: strings.TrimSpace(in.Description), Body: in.Body, Enabled: in.Enabled}
	if err := store.Save(sk); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": store.Dir()})
}

// handleDeleteSkill removes a skill file (idempotent).
func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	store, err := s.engine().Skills()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := store.Delete(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
