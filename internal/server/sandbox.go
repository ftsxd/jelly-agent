package server

import (
	"net/http"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

// sandboxView is the GET /api/sandbox payload: the persisted policy plus the
// effective defaults (so blank fields show what they fall back to) and whether
// a docker binary is present (so the UI can warn before selecting docker).
type sandboxView struct {
	Backend         string `json:"backend"`
	AllowDocker     bool   `json:"allow_docker"`
	Network         bool   `json:"network"`
	Image           string `json:"image"`
	TimeoutSec      int    `json:"timeout_sec"`
	MaxOutputKB     int    `json:"max_output_kb"`
	CPUSeconds      int    `json:"cpu_seconds"`
	MaxProcs        int    `json:"max_procs"`
	MemoryMB        int    `json:"memory_mb"`
	DockerAvailable bool   `json:"docker_available"`
	Defaults        struct {
		Image       string `json:"image"`
		TimeoutSec  int    `json:"timeout_sec"`
		MaxOutputKB int    `json:"max_output_kb"`
		CPUSeconds  int    `json:"cpu_seconds"`
		MaxProcs    int    `json:"max_procs"`
		MemoryMB    int    `json:"memory_mb"`
	} `json:"defaults"`
}

// handleSandbox returns the current sandbox policy (the execution envelope for
// skill scripts) so the web UI can edit it without touching config.yaml.
func (s *Server) handleSandbox(w http.ResponseWriter, _ *http.Request) {
	sb := s.engine().Config().Sandbox
	var v sandboxView
	v.Backend = sb.Backend
	v.AllowDocker = sb.AllowDocker
	v.Network = sb.Network
	v.Image = sb.Image
	v.TimeoutSec = sb.TimeoutSec
	v.MaxOutputKB = sb.MaxOutputKB
	v.CPUSeconds = sb.CPUSeconds
	v.MaxProcs = sb.MaxProcs
	v.MemoryMB = sb.MemoryMB
	v.DockerAvailable = sandbox.DockerAvailable()
	v.Defaults.Image = sandbox.DefaultImage
	v.Defaults.TimeoutSec = int(sandbox.DefaultTimeout.Seconds())
	v.Defaults.MaxOutputKB = sandbox.DefaultMaxOutput >> 10
	v.Defaults.CPUSeconds = sandbox.DefaultCPUSecs
	v.Defaults.MaxProcs = sandbox.DefaultMaxProcs
	v.Defaults.MemoryMB = sandbox.DefaultMemoryMB
	writeJSON(w, http.StatusOK, v)
}

// sandboxInput is the POST /api/sandbox body. Numeric zero ⇒ "use the sandbox
// default" (persisted as an omitted field), matching the config semantics.
type sandboxInput struct {
	Backend     string `json:"backend"`
	AllowDocker bool   `json:"allow_docker"`
	Network     bool   `json:"network"`
	Image       string `json:"image"`
	TimeoutSec  int    `json:"timeout_sec"`
	MaxOutputKB int    `json:"max_output_kb"`
	CPUSeconds  int    `json:"cpu_seconds"`
	MaxProcs    int    `json:"max_procs"`
	MemoryMB    int    `json:"memory_mb"`
}

// handleSetSandbox persists the sandbox policy and hot-reloads the engine.
func (s *Server) handleSetSandbox(w http.ResponseWriter, r *http.Request) {
	var in sandboxInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	switch in.Backend {
	case "", "native", "docker":
	default:
		writeErr(w, http.StatusBadRequest, `backend 只能是 ""、native 或 docker`)
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
	raw.Sandbox = config.Sandbox{
		Backend:     in.Backend,
		AllowDocker: in.AllowDocker,
		Network:     in.Network,
		Image:       in.Image,
		TimeoutSec:  in.TimeoutSec,
		MaxOutputKB: in.MaxOutputKB,
		CPUSeconds:  in.CPUSeconds,
		MaxProcs:    in.MaxProcs,
		MemoryMB:    in.MemoryMB,
	}
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}
