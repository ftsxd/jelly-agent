package engine

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/codeproject"
	"github.com/jelly-agent/jelly-agent/internal/config"
)

// CodeProjects lives beside the config, outside globally shared code roots.
// Reopening returns the same store through engine reloads, so grants are live.
func (e *Engine) CodeProjects() *codeproject.Store {
	path := e.cfg.SourcePath
	if path == "" || path == "(env)" {
		path, _ = config.DefaultUserConfigPath()
	}
	store := codeproject.Open(filepath.Join(filepath.Dir(path), "code-projects"))
	// Open returns the same Store across reloads, so the limits are reapplied
	// on every lookup rather than only at construction — that is what makes a
	// config edit take effect without a restart.
	c := e.cfg.Files.CodeProjects
	store.SetAutoProbe(!c.NoRemoteCheck)
	store.SetLimits(codeproject.Limits{
		SyncTimeout:      time.Duration(c.SyncTimeoutSec) * time.Second,
		MaxSnapshotBytes: int64(c.MaxSnapshotMB) << 20,
		MaxSnapshotFiles: c.MaxSnapshotFiles,
		HistoryDepth:     c.HistoryDepth,
	})
	return store
}

// sharedCodeRoots cannot expose managed snapshots through the legacy global
// file tools, even if a broad root was configured directly in YAML.
func (e *Engine) sharedCodeRoots() []string {
	protected := filepath.Dir(e.CodeProjects().Dir())
	canonical := func(p string) string {
		p, _ = filepath.Abs(p)
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	protected = canonical(protected)
	out := []string{}
	for _, root := range e.cfg.Files.Roots {
		abs := canonical(root)
		if pathContains(abs, protected) || pathContains(protected, abs) {
			continue
		}
		out = append(out, root)
	}
	return out
}
func pathContains(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
