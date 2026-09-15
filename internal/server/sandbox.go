package server

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

// sandboxView is the GET /api/sandbox payload: the persisted policy plus the
// effective defaults (so blank fields show what they fall back to) and whether
// a docker binary is present (so the UI can warn before selecting docker).
type sandboxView struct {
	Mode            string   `json:"mode"`
	Backend         string   `json:"backend"`
	AllowDocker     bool     `json:"allow_docker"`
	Network         bool     `json:"network"`
	ReadPaths       []string `json:"read_paths"`
	WritePaths      []string `json:"write_paths"`
	Image           string   `json:"image"`
	TimeoutSec      int      `json:"timeout_sec"`
	MaxOutputKB     int      `json:"max_output_kb"`
	CPUSeconds      int      `json:"cpu_seconds"`
	MaxProcs        int      `json:"max_procs"`
	MemoryMB        int      `json:"memory_mb"`
	DockerAvailable bool     `json:"docker_available"`
	// What the lightweight OS sandbox can actually enforce on this host. The
	// Linux answer depends on the running kernel, so the UI has to be told
	// rather than assume — a policy that silently enforces less than it claims
	// is worse than one that admits it.
	OSAvailable bool     `json:"os_available"`
	OSDetail    string   `json:"os_detail"`
	Modes       []string `json:"modes"`
	Defaults    struct {
		Mode        string `json:"mode"`
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
func (s *Server) handleSandbox(w http.ResponseWriter, r *http.Request) {
	sb := s.engineFor(r).Config().Sandbox
	var v sandboxView
	v.Mode = sb.Mode
	v.Backend = sb.Backend
	v.AllowDocker = sb.AllowDocker
	v.Network = sb.Network
	v.ReadPaths = sb.ReadPaths
	v.WritePaths = sb.WritePaths
	v.Image = sb.Image
	v.TimeoutSec = sb.TimeoutSec
	v.MaxOutputKB = sb.MaxOutputKB
	v.CPUSeconds = sb.CPUSeconds
	v.MaxProcs = sb.MaxProcs
	v.MemoryMB = sb.MemoryMB
	v.DockerAvailable = sandbox.DockerAvailable()
	v.OSAvailable = sandbox.OSSandboxAvailable()
	v.OSDetail = sandbox.OSSandboxDetail()
	for _, m := range sandbox.Modes {
		v.Modes = append(v.Modes, string(m))
	}
	v.Defaults.Mode = string(sandbox.DefaultMode)
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
	Mode        string   `json:"mode"`
	Backend     string   `json:"backend"`
	AllowDocker bool     `json:"allow_docker"`
	Network     bool     `json:"network"`
	ReadPaths   []string `json:"read_paths"`
	WritePaths  []string `json:"write_paths"`
	Image       string   `json:"image"`
	TimeoutSec  int      `json:"timeout_sec"`
	MaxOutputKB int      `json:"max_output_kb"`
	CPUSeconds  int      `json:"cpu_seconds"`
	MaxProcs    int      `json:"max_procs"`
	MemoryMB    int      `json:"memory_mb"`
}

// handleSetSandbox persists the sandbox policy and hot-reloads the engine.
func (s *Server) handleSetSandbox(w http.ResponseWriter, r *http.Request) {
	var in sandboxInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	switch in.Backend {
	case "", "native", "os", "docker":
	default:
		writeErr(w, http.StatusBadRequest, `backend 只能是 ""、native、os 或 docker`)
		return
	}
	if in.Mode != "" && !sandbox.Mode(in.Mode).Valid() {
		writeErr(w, http.StatusBadRequest, "mode 只能是 "+modeNames()+" 之一")
		return
	}
	// An absolute path is the only thing that means the same to the confinement
	// layer as it does to whoever typed it — a relative one would be resolved
	// against the server's cwd, which is not where the operator is looking.
	for _, group := range []struct {
		field string
		paths []string
	}{
		{"read_paths", in.ReadPaths},
		{"write_paths", in.WritePaths},
	} {
		for _, rp := range group.paths {
			if !filepath.IsAbs(rp) {
				writeErr(w, http.StatusBadRequest, group.field+" 必须是绝对路径："+rp)
				return
			}
		}
	}

	path, raw, done, err := s.editConfig()
	defer done()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw.Sandbox = config.Sandbox{
		Mode:        in.Mode,
		Backend:     in.Backend,
		AllowDocker: in.AllowDocker,
		Network:     in.Network,
		ReadPaths:   in.ReadPaths,
		WritePaths:  in.WritePaths,
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

// modeNames renders the valid sandbox modes for an error message.
func modeNames() string {
	names := make([]string, 0, len(sandbox.Modes))
	for _, m := range sandbox.Modes {
		names = append(names, string(m))
	}
	return strings.Join(names, "、")
}
