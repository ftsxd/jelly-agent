package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/codeproject"
)

func projectError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, codeproject.ErrNotFound) {
		code = http.StatusNotFound
	}
	if errors.Is(err, codeproject.ErrBusy) {
		code = http.StatusConflict
	}
	writeErr(w, code, err.Error())
}
func (s *Server) handleCodeProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.engineFor(r).CodeProjects().List()
	if err != nil {
		writeErr(w, 500, "无法读取项目配置")
		return
	}
	for i := range ps {
		ps[i].Snapshot = ""
	}
	writeJSON(w, 200, map[string]any{"projects": ps})
}
func (s *Server) validProjectGrants(w http.ResponseWriter, r *http.Request, grants []codeproject.Grant) bool {
	cfg := s.engine().Config()
	for _, g := range grants {
		if g.Agent == "root" {
			continue
		}
		found := false
		for _, a := range cfg.Agents {
			if a.Name == g.Agent {
				found = true
				break
			}
		}
		if !found {
			writeErr(w, 400, "授权 Agent 不存在："+g.Agent)
			return false
		}
	}
	return true
}

// saveProjectInput is the project plus a write-only credential. The token is
// deliberately not a field on codeproject.Project: the struct that gets
// serialized into projects.json and handed to the console is not a place a
// secret can end up by accident.
type saveProjectInput struct {
	codeproject.Project
	// Token, when non-empty, replaces the stored credential. Omitted or empty
	// leaves it alone, so editing a project does not silently wipe it — the
	// console cannot re-send a value it is never allowed to read back.
	Token string `json:"token,omitempty"`
	// ClearToken removes it. Clearing has to be explicit for the same reason.
	ClearToken bool `json:"clear_token,omitempty"`
}

func (s *Server) handleSaveCodeProject(w http.ResponseWriter, r *http.Request) {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	var in saveProjectInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p := in.Project
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.URL = strings.TrimSpace(p.URL)
	p.Branch = strings.TrimSpace(p.Branch)
	p.TokenEnv = strings.TrimSpace(p.TokenEnv)
	token := strings.TrimSpace(in.Token)
	if len(token) > 4096 {
		writeErr(w, 400, "访问令牌过长")
		return
	}
	if !s.validProjectGrants(w, r, p.Grants) {
		return
	}
	store := s.engineFor(r).CodeProjects()
	if err := store.Save(p); err != nil {
		projectError(w, err)
		return
	}
	if in.ClearToken || token != "" {
		if in.ClearToken {
			token = ""
		}
		if err := store.SetToken(p.ID, token); err != nil {
			writeErr(w, 500, "项目已保存，但访问令牌写入失败")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) handleCodeProjectGrants(w http.ResponseWriter, r *http.Request) {
	// Serialize validation with agent deletion so a stale request cannot
	// recreate a grant after the agent's grants have been revoked.
	s.editMu.Lock()
	defer s.editMu.Unlock()
	var in struct {
		Grants []codeproject.Grant `json:"grants"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.validProjectGrants(w, r, in.Grants) {
		return
	}
	if err := s.engineFor(r).CodeProjects().SetGrants(r.PathValue("id"), in.Grants); err != nil {
		projectError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) handleDeleteCodeProject(w http.ResponseWriter, r *http.Request) {
	if err := s.engineFor(r).CodeProjects().Delete(r.PathValue("id")); err != nil {
		projectError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleSyncCodeProject starts the pull and returns. It deliberately does not
// wait: a full monorepo checkout outlives the browser tab, the reverse proxy
// and the read timeout, and running the clone on r.Context() meant every one of
// those cancelled it. The task id lets the page follow along afterwards.
// handleCheckCodeProjectUpdate answers "is this snapshot still current" with one
// ls-remote instead of a 352 MB clone.
func (s *Server) handleCheckCodeProjectUpdate(w http.ResponseWriter, r *http.Request) {
	status, err := s.engineFor(r).CodeProjects().CheckRemote(r.Context(), r.PathValue("id"))
	if err != nil {
		projectError(w, err)
		return
	}
	writeJSON(w, 200, status)
}

func (s *Server) handleSyncCodeProject(w http.ResponseWriter, r *http.Request) {
	taskID, err := s.engineFor(r).CodeProjects().StartSync(r.PathValue("id"))
	if err != nil {
		projectError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "task_id": taskID})
}

// handleResolveSyncRequest answers an agent's ask that a project be pulled.
//
// Approval is a person's click, not a tool call: the pull uses the stored
// credential, takes minutes on a monorepo and replaces the tree that every
// later answer cites. The agent can say a snapshot is stale and why — which is
// the part it is actually in a position to know — and the decision stays here.
func (s *Server) handleResolveSyncRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	store := s.engineFor(r).CodeProjects()
	approve := r.Method == http.MethodPost
	// Cleared first either way: an approval that starts the pull and then
	// fails to clear would leave a card asking for something already running.
	if err := store.ClearSyncRequest(id); err != nil {
		projectError(w, err)
		return
	}
	if !approve {
		writeJSON(w, 200, map[string]any{"ok": true, "approved": false})
		return
	}
	taskID, err := store.StartSync(id)
	if err != nil {
		projectError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "approved": true, "task_id": taskID})
}

// Annotations are edited one row at a time rather than as one big document:
// a monorepo project can hold a label per service, and resending the whole
// catalogue to rename one of them is the kind of thing that silently drops the
// other 129 when two tabs are open.
func (s *Server) handleSetCodeProjectAnnotation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string                    `json:"path"`
		Info codeproject.DirectoryInfo `json:"info"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.engineFor(r).CodeProjects().SetAnnotation(r.PathValue("id"), in.Path, in.Info); err != nil {
		projectError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleResolveCodeProjectDrafts accepts agent-written drafts into the
// catalogue, or discards them. Nothing an agent proposed reaches the read tools
// until it comes through here.
func (s *Server) handleResolveCodeProjectDrafts(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Accept    []string `json:"accept,omitempty"`
		Reject    []string `json:"reject,omitempty"`
		AcceptAll bool     `json:"accept_all,omitempty"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	n, err := s.engineFor(r).CodeProjects().ResolveDrafts(r.PathValue("id"), in.Accept, in.Reject, in.AcceptAll)
	if err != nil {
		projectError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "accepted": n})
}
