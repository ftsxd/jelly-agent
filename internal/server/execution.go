package server

import (
	"net/http"

	"github.com/jelly-agent/jelly-agent/internal/execution"
	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

func (s *Server) handleExecution(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"config":        s.engineFor(r).ExecutionConfig(),
		"default_rules": execution.DefaultRules(),
		"defaults":      map[string]any{"timeout_sec": execution.DefaultTimeoutSec, "max_output_kb": execution.DefaultMaxOutputKB},
		"os_available":  sandbox.OSSandboxAvailable(), "os_detail": sandbox.OSSandboxDetail(),
		"docker_available": sandbox.DockerAvailable(),
	})
}

func (s *Server) handleSetExecution(w http.ResponseWriter, r *http.Request) {
	var in execution.Config
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	path, raw, done, err := s.editConfig()
	defer done()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// An unknown identity is usually a typo, and should not look assigned.
	known := map[string]bool{"root": true}
	for _, a := range raw.Agents {
		known[a.Name] = true
	}
	for _, p := range in.Profiles {
		for _, a := range p.Agents {
			if !known[a] {
				writeErr(w, http.StatusBadRequest, "不存在的 Agent："+a)
				return
			}
		}
	}
	raw.Execution = in
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}

// Policy check only: no credentials are resolved and no child is started.
func (s *Server) handleCheckExecution(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Agent string `json:"agent"`
		execution.Request
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Agent == "" {
		in.Agent = "root"
	}
	runtime := execution.Runtime{Config: s.engineFor(r).ExecutionConfig()}
	writeJSON(w, http.StatusOK, runtime.Check(in.Agent, in.Request))
}
