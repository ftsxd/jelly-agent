package server

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/tool"
)

// maxProbedProjects caps how many child directories are reported per root. The
// listing is there to confirm a path points where the operator thinks it does,
// not to browse a monorepo.
const maxProbedProjects = 50

// codeRootView is one configured code directory plus what is actually there.
// A path list with no feedback is where typos go unnoticed: the tools simply
// stay silent, and the agent reports that it cannot find any code.
type codeRootView struct {
	Path     string   `json:"path"`
	Exists   bool     `json:"exists"`
	Resolved string   `json:"resolved,omitempty"` // set when a symlink made it differ
	Projects []string `json:"projects,omitempty"` // immediate child directories
	More     bool     `json:"more,omitempty"`     // more children than reported
	Error    string   `json:"error,omitempty"`
}

// codeView is the GET /api/files payload.
type codeView struct {
	Roots []codeRootView `json:"roots"`
	// ToolsEnabled says whether the file tools are actually registered right
	// now — the honest answer to "did my configuration take effect", which is
	// not the same as "I typed a path".
	ToolsEnabled bool     `json:"tools_enabled"`
	Tools        []string `json:"tools"`
	SkipDirs     []string `json:"skip_dirs"`
}

// handleFiles reports the configured code directories and their live state.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	cfg := s.engineFor(r).Config()
	v := codeView{
		Roots:        []codeRootView{},
		ToolsEnabled: !s.engineFor(r).FileRoots().Empty(),
		Tools:        []string{"read_file", "list_dir", "grep_files"},
		SkipDirs:     tool.SkipDirNames(),
	}
	for _, p := range cfg.Files.Roots {
		v.Roots = append(v.Roots, probeRoot(p))
	}
	writeJSON(w, http.StatusOK, v)
}

// probeRoot answers, for one configured path: is it there, what does it really
// resolve to, and which projects sit inside it.
func probeRoot(p string) codeRootView {
	out := codeRootView{Path: p}
	abs, err := filepath.Abs(p)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		out.Error = "目录不存在（同步任务还没跑过？）"
		return out
	}
	info, err := os.Stat(resolved)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if !info.IsDir() {
		out.Error = "不是目录"
		return out
	}
	out.Exists = true
	if resolved != abs {
		out.Resolved = resolved
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if len(out.Projects) >= maxProbedProjects {
			out.More = true
			break
		}
		out.Projects = append(out.Projects, e.Name())
	}
	sort.Strings(out.Projects)
	return out
}

// codeInput is the POST /api/files body.
type codeInput struct {
	Roots []string `json:"roots"`
}

// handleSetFiles persists the code directories and hot-reloads. Saving an empty
// list is a valid choice: it takes the file tools away again.
func (s *Server) handleSetFiles(w http.ResponseWriter, r *http.Request) {
	var in codeInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	roots := cleanNames(in.Roots)
	for _, p := range roots {
		// Absolute only: a relative path would resolve against the server's
		// working directory, which is not where the operator is looking.
		if !filepath.IsAbs(p) {
			writeErr(w, http.StatusBadRequest, "代码目录必须是绝对路径："+p)
			return
		}
	}

	path, raw, done, err := s.editConfig()
	defer done()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Guard the one mistake containment cannot undo. The tools resolve symlinks
	// before checking bounds, so a repository can never escape its root — but a
	// root that IS the agent's own state directory hands over the API keys and
	// the session database by definition, and no amount of path checking helps.
	stateDir := filepath.Dir(path)
	for _, p := range roots {
		if within(stateDir, p) || within(p, stateDir) {
			writeErr(w, http.StatusBadRequest,
				"拒绝把 agent 自己的配置/状态目录设为代码目录（"+stateDir+"）：那里面是 API key 和会话库")
			return
		}
	}

	raw.Files = config.Files{Roots: roots}
	if err := s.persist(w, raw, path); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
}

// within reports whether candidate is dir itself or sits inside it, comparing
// resolved paths so a symlinked candidate cannot slip past.
func within(dir, candidate string) bool {
	resolve := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	rel, err := filepath.Rel(resolve(dir), resolve(candidate))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel))
}
