package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/skill"
)

// maxSkillZipUpload caps the uploaded zip size (the extracted content is capped
// separately inside skill.ImportZip).
const maxSkillZipUpload = 10 << 20 // 10 MiB

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
	writeJSON(w, http.StatusOK, map[string]any{
		"skills":        out,
		"dir":           store.Dir(),
		"allow_scripts": s.engine().Config().Skills.AllowScripts,
	})
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
	// var_keys lists configured variable names only (values masked); scripts
	// lists runnable bundled files for directory-form skills.
	cfg := s.engine().Config()
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        sk.Name,
		"description": sk.Description,
		"enabled":     sk.Enabled,
		"body":        sk.Body,
		"var_keys":    sortedKeys(cfg.SkillVars[sk.Name]),
		"scripts":     store.Scripts(sk.Name),
	})
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

// handleUploadSkill imports a skill from an uploaded .zip (multipart field
// "file"): the zip must contain a SKILL.md whose frontmatter name becomes the
// skill id; bundled files are extracted alongside it.
func (s *Server) handleUploadSkill(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxSkillZipUpload); err != nil {
		writeErr(w, http.StatusBadRequest, "解析上传失败: "+err.Error())
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "缺少上传文件字段 file")
		return
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxSkillZipUpload+1))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(data) > maxSkillZipUpload {
		writeErr(w, http.StatusRequestEntityTooLarge, "zip 文件过大（上限 10 MiB）")
		return
	}

	store, err := s.engine().Skills()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sk, err := store.ImportZip(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": sk.Name, "description": sk.Description})
}

// skillVarsInput sets a skill's variables (key/value). An empty value keeps the
// stored one (so editing without re-typing a secret preserves it).
type skillVarsInput struct {
	Vars map[string]string `json:"vars"`
}

// handleSetSkillVars merges variables into a skill's config-stored variable set
// (config.yaml, 0600), then persists + hot-reloads. Secret values never go to
// the model or the skill files — only into run_script's environment.
func (s *Server) handleSetSkillVars(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !skill.ValidName(name) {
		writeErr(w, http.StatusBadRequest, "技能名非法")
		return
	}
	var in skillVarsInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	path, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := loadRawOrEmpty(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw.SkillVars == nil {
		raw.SkillVars = map[string]map[string]string{}
	}
	raw.SkillVars[name] = mergeSecrets(raw.SkillVars[name], in.Vars) // empty values keep existing
	if len(raw.SkillVars[name]) == 0 {
		delete(raw.SkillVars, name)
	}
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "var_keys": sortedKeys(raw.SkillVars[name])})
}

// handleDeleteSkillVar removes one variable from a skill.
func (s *Server) handleDeleteSkillVar(w http.ResponseWriter, r *http.Request) {
	name, key := r.PathValue("name"), r.PathValue("key")
	path, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := loadRawOrEmpty(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw.SkillVars[name] != nil {
		delete(raw.SkillVars[name], key)
		if len(raw.SkillVars[name]) == 0 {
			delete(raw.SkillVars, name)
		}
	}
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetAllowScripts toggles whether skills may execute bundled scripts
// (the run_script tool). Off by default — it runs code with the user's
// privileges, so enabling is an explicit, persisted choice.
func (s *Server) handleSetAllowScripts(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	path, err := s.writeTargetPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := loadRawOrEmpty(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw.Skills.AllowScripts = in.Enabled
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "allow_scripts": s.engine().Config().Skills.AllowScripts})
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
