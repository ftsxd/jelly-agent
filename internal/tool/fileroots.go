package tool

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Roots is the set of directories the file tools may reach, and the only thing
// standing between a model that can read files and the rest of the host. Every
// path the model supplies is resolved through it.
//
// Containment is checked AFTER resolving symlinks, which is the part that is
// easy to get wrong: a synced repository is attacker-influenced content, and a
// checked-in symlink pointing at /etc or at the agent's own config directory
// would otherwise be followed happily. Resolving first and comparing second is
// what makes "inside a root" mean what it says.
type Roots struct {
	dirs []string // absolute, symlink-resolved, no trailing separator
	// prefixes narrows a single root to a set of repo-relative directories.
	// Empty means the whole tree. It is a second containment check layered on
	// top of the root, not a replacement: a path must be inside the root AND
	// inside a prefix. Kept as slash paths relative to dirs[0].
	//
	// This is why a scoped project keeps exactly one root even though it
	// exposes several directories. Handing each directory in as its own root
	// would make Display prefix every path with the directory's base name, and
	// the model would read back paths that are no longer repo-relative — which
	// is the one property that lets it cite services/order/handler.go and have
	// that mean something outside this tool.
	prefixes []string
}

// NewScopedRoot is NewRoots for a single directory narrowed to a set of
// repo-relative prefixes. A nil/empty prefix list, or one containing ".", means
// the whole tree — which is what a project configured for whole-repo analysis
// gets, and what every project created before directory scoping existed gets.
func NewScopedRoot(dir string, prefixes []string) Roots {
	r := NewRoots([]string{dir})
	if r.Empty() {
		return r
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range prefixes {
		p = path.Clean(strings.TrimSpace(strings.ReplaceAll(p, "\\", "/")))
		if p == "" || p == "." {
			return r // whole tree; no narrowing
		}
		if p == ".." || strings.HasPrefix(p, "../") || path.IsAbs(p) {
			continue // a caller-supplied traversal is dropped, never honoured
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return r
	}
	r.prefixes = out
	return r
}

// rel returns abs as a slash path relative to the root, and whether it is
// inside. Only meaningful for a scoped (single-root) Roots.
func (r Roots) rel(abs string) (string, bool) {
	if len(r.dirs) == 0 {
		return "", false
	}
	rel, err := filepath.Rel(r.dirs[0], abs)
	if err != nil || escapes(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// inPrefix reports whether a repo-relative path is at or below a configured
// prefix. "services/order" allows "services/order" and "services/order/x.go",
// but not "services/orders" — the separator check is what keeps a sibling
// directory with a shared name prefix out.
func inPrefix(rel string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// AllowsDir reports whether an absolute path inside the root is itself within
// the configured scope.
func (r Roots) AllowsDir(abs string) bool {
	if len(r.prefixes) == 0 {
		return true
	}
	rel, ok := r.rel(abs)
	return ok && inPrefix(rel, r.prefixes)
}

// IsNavigation reports whether abs is a strict ancestor of some configured
// prefix — "services" when the scope is "services/order". Such a directory is
// traversable so the model can walk down to its scope, but listing it must show
// only the child that leads there, never the other 125 siblings.
func (r Roots) IsNavigation(abs string) bool {
	if len(r.prefixes) == 0 {
		return false
	}
	rel, ok := r.rel(abs)
	if !ok {
		return false
	}
	if rel == "." {
		return true
	}
	for _, p := range r.prefixes {
		if strings.HasPrefix(p, rel+"/") {
			return true
		}
	}
	return false
}

// NavigationChildren returns the immediate child directory names that lead
// toward a configured prefix, for a path IsNavigation accepted.
func (r Roots) NavigationChildren(abs string) []string {
	rel, ok := r.rel(abs)
	if !ok {
		return nil
	}
	base := ""
	if rel != "." {
		base = rel + "/"
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range r.prefixes {
		if !strings.HasPrefix(p, base) {
			continue
		}
		child := strings.SplitN(strings.TrimPrefix(p, base), "/", 2)[0]
		if child != "" && !seen[child] {
			seen[child] = true
			out = append(out, child)
		}
	}
	return out
}

// Scope returns the configured prefixes, empty for an unscoped Roots.
func (r Roots) Scope() []string { return append([]string(nil), r.prefixes...) }

// NewRoots canonicalizes the configured roots. A root that does not exist is
// dropped rather than fatal — a code directory that has not been synced yet is
// a normal state at first boot, not a misconfiguration.
func NewRoots(dirs []string) Roots {
	var out []string
	seen := map[string]bool{}
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			continue // not there yet
		}
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
			continue
		}
		if !seen[resolved] {
			seen[resolved] = true
			out = append(out, resolved)
		}
	}
	return Roots{dirs: out}
}

// Empty reports whether no usable root is configured. The file tools are not
// registered at all in that case: a tool that can only ever answer "no such
// root" is worse than an absent one, because the model will keep trying.
func (r Roots) Empty() bool { return len(r.dirs) == 0 }

// Dirs returns the resolved roots, for display and for handing to the sandbox
// as readable paths.
func (r Roots) Dirs() []string { return append([]string(nil), r.dirs...) }

// Resolve turns a model-supplied path into an absolute path inside a root.
//
// An absolute path must already be inside one. A relative path is tried against
// each root in order and the first match that exists wins, so with a single root
// the model can simply say "project-a/internal/x.go".
func (r Roots) Resolve(p string) (string, error) {
	if r.Empty() {
		return "", fmt.Errorf("没有配置可访问的代码目录（files.roots）")
	}
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("path 不能为空")
	}

	if filepath.IsAbs(p) {
		abs, err := resolveExisting(p)
		if err != nil {
			return "", err
		}
		if !r.contains(abs) {
			return "", r.outsideErr(p)
		}
		return abs, nil
	}

	var firstErr error
	for _, root := range r.dirs {
		abs, err := resolveExisting(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// Still check containment: the join may land inside the root while the
		// symlink resolution walks back out of it.
		if r.contains(abs) {
			return abs, nil
		}
		return "", r.outsideErr(p)
	}
	if firstErr != nil {
		return "", fmt.Errorf("路径不存在: %s", p)
	}
	return "", r.outsideErr(p)
}

// ResolveOrRoots is Resolve, except that an empty path means "everywhere": it
// returns all roots. grep_files uses it so a search with no path given covers
// every project instead of failing.
func (r Roots) ResolveOrRoots(p string) ([]string, error) {
	if r.Empty() {
		return nil, fmt.Errorf("没有配置可访问的代码目录（files.roots）")
	}
	if strings.TrimSpace(p) == "" {
		if len(r.prefixes) == 0 {
			return r.Dirs(), nil
		}
		// Searching "everywhere" in a scoped project means the configured
		// directories, not the repository they happen to sit in.
		out := []string{}
		for _, pre := range r.prefixes {
			abs := filepath.Join(r.dirs[0], filepath.FromSlash(pre))
			if info, err := os.Stat(abs); err == nil && info.IsDir() {
				out = append(out, abs)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("项目配置的目录都不存在，请检查目录配置后重新同步")
		}
		return out, nil
	}
	abs, err := r.Resolve(p)
	if err != nil {
		return nil, err
	}
	return []string{abs}, nil
}

// Display renders an absolute path the way the model should see it: relative to
// its root, so the paths it reads back are the paths it can pass in again.
func (r Roots) Display(abs string) string {
	for _, root := range r.dirs {
		if rel, err := filepath.Rel(root, abs); err == nil && !escapes(rel) {
			if len(r.dirs) == 1 {
				return filepath.ToSlash(rel)
			}
			return filepath.ToSlash(filepath.Join(filepath.Base(root), rel))
		}
	}
	return abs
}

func (r Roots) contains(abs string) bool {
	for _, root := range r.dirs {
		rel, err := filepath.Rel(root, abs)
		if err != nil || escapes(rel) {
			continue
		}
		if len(r.prefixes) == 0 {
			return true
		}
		slash := filepath.ToSlash(rel)
		// A navigation ancestor resolves so the model can walk down to its
		// scope; listDir is what keeps the listing itself narrow.
		return inPrefix(slash, r.prefixes) || r.IsNavigation(abs)
	}
	return false
}

func (r Roots) outsideErr(p string) error {
	return fmt.Errorf("路径越界，只能访问已配置的代码目录 %v：%s", r.dirs, p)
}

// resolveExisting resolves symlinks on a path that must already exist.
func resolveExisting(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("路径不存在: %s", p)
	}
	return resolved, nil
}

// escapes reports whether a filepath.Rel result leaves its base.
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel)
}
