package codeproject

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/logging"
)

// Only Git objects live here; agent tools can only resolve snapshot-* trees.
// No remote or credential is stored in Git config. The identity marker prevents
// reuse after a repository/branch change, including an interrupted config edit.
type gitCacheState struct {
	URL     string `json:"url"`
	Branch  string `json:"branch"`
	Fetches int    `json:"fetches"`
}

// gcBudget bounds one cache housekeeping pass. A var so a test can drive the
// path where it does not finish in time.
var gcBudget = 30 * time.Second

func (s *Store) gitCachePath(id string) string {
	return filepath.Join(s.dir, "cache-"+id+".git")
}

func (s *Store) removeGitCache(id string) {
	if identifier.MatchString(id) {
		_ = os.RemoveAll(s.gitCachePath(id))
	}
}

func (s *Store) syncSnapshot(ctx context.Context, p Project, limits Limits) (string, string, error) {
	g, err := s.newGitSession(p)
	if err != nil {
		return "", "", err
	}
	defer g.cleanup()
	cache, revision, err := s.fetchCache(ctx, g, p, limits)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", fmt.Errorf("拉取已取消或超过 %s，请重试或调高 files.code_projects.sync_timeout_sec", limits.SyncTimeout)
		}
		return "", "", err
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if revision == p.Revision && filepath.Base(p.Snapshot) == p.Snapshot && strings.HasPrefix(p.Snapshot, "snapshot-") {
		if info, err := os.Lstat(filepath.Join(s.dir, p.Snapshot)); err == nil && info.IsDir() {
			return p.Snapshot, revision, nil
		}
	}
	checkout := filepath.Join(g.work, "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		return "", "", err
	}
	if err := g.exportSnapshot(ctx, cache, revision, checkout, limits); err != nil {
		return "", "", err
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	dest, err := os.MkdirTemp(s.dir, "snapshot-")
	if err != nil {
		return "", "", err
	}
	if err := os.Remove(dest); err != nil {
		return "", "", err
	}
	if err := os.Rename(checkout, dest); err != nil {
		return "", "", err
	}
	return filepath.Base(dest), revision, nil
}

func (s *Store) fetchCache(ctx context.Context, g *gitSession, p Project, limits Limits) (string, string, error) {
	cache := s.gitCachePath(p.ID)
	var state gitCacheState
	b, err := os.ReadFile(filepath.Join(cache, "codeproject.json"))
	valid := err == nil && json.Unmarshal(b, &state) == nil && state.URL == p.URL && state.Branch == p.Branch
	if valid {
		bare, err := g.run(ctx, "-C", cache, "rev-parse", "--is-bare-repository")
		valid = err == nil && bare == "true"
	}
	repo := cache
	if !valid {
		repo = filepath.Join(g.work, "cache.git")
		if _, err := g.run(ctx, "init", "--bare", "--", repo); err != nil {
			return "", "", errors.New(diagnose(g.stderr.b.String(), g.token, g.auth, p, err))
		}
		state = gitCacheState{URL: p.URL, Branch: p.Branch}
	}
	// A fixed local ref accepts force-pushes without retaining other branches.
	// An empty cache fetches the full tip tree; later fetches negotiate against
	// the objects already present. Never export stale FETCH_HEAD after failure.
	//
	// The depth is what the history tools read: at depth 1 there is a snapshot
	// and nothing to say about how it got that way, which is the state that
	// made an agent answer 我没有 git log/diff 能力. Deepening an existing
	// shallow cache is itself incremental — git asks for the commits it is
	// missing, not for the tree again.
	if _, err := g.run(ctx, "-C", repo, "fetch", "--depth="+strconv.Itoa(historyDepth(p, limits)), "--no-tags", "--no-recurse-submodules", "--no-auto-gc", "--", p.URL, "+refs/heads/"+p.Branch+":refs/heads/snapshot"); err != nil {
		return "", "", errors.New(diagnose(g.stderr.b.String(), g.token, g.auth, p, err))
	}
	revision, err := g.run(ctx, "-C", repo, "rev-parse", "--verify", "refs/heads/snapshot^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("无法读取代码版本")
	}
	// Release archives may omit source files or substitute their contents. An
	// analysis snapshot must include those files and keep their original bytes.
	if err := os.MkdirAll(filepath.Join(repo, "info"), 0700); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(repo, "info", "attributes"), []byte("* -export-ignore -export-subst\n"), 0600); err != nil {
		return "", "", err
	}
	state.Fetches++
	// Shallow fetch does not remove old objects. All cache users hold the
	// project reservation, so pruning here cannot race an export or fetch.
	if state.Fetches >= 32 {
		gcCtx, cancel := context.WithTimeout(ctx, gcBudget)
		lock := s.cacheLock(p.ID)
		lock.Lock()
		_, gcErr := g.run(gcCtx, "-C", repo, "gc", "--prune=now")
		lock.Unlock()
		cancel()
		// The counter resets whether or not that worked. A gc that cannot
		// finish inside the budget will not finish inside it next time either,
		// and retrying on every sync turns housekeeping into a fixed 30-second
		// tax on the very thing the cache exists to make fast — visible only as
		// "同步莫名其妙变慢了". Skipping it costs some disk until the next
		// window, which is the cheaper of the two.
		state.Fetches = 0
		if gcErr != nil {
			slog.Warn("代码缓存回收未完成，已跳过，下一轮再试",
				"project", p.ID, "budget", gcBudget.String(), logging.Err(gcErr))
		}
	}
	b, err = json.Marshal(state)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(repo, "codeproject.json"), b, 0600); err != nil {
		return "", "", err
	}
	if repo != cache {
		lock := s.cacheLock(p.ID)
		lock.Lock()
		defer lock.Unlock()
		if err := os.RemoveAll(cache); err != nil {
			return "", "", err
		}
		if err := os.Rename(repo, cache); err != nil {
			return "", "", err
		}
	}
	return cache, revision, nil
}

func (g *gitSession) exportSnapshot(ctx context.Context, cache, revision, dest string, limits Limits) error {
	cmd := g.command(ctx, "-C", cache, "archive", "--format=tar", revision)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := extractSnapshot(ctx, stdout, dest, limits)
	if extractErr == nil {
		// tar EOF can precede the producer's final padding bytes. Drain them
		// before waiting so a valid archive cannot fail with a broken pipe.
		_, extractErr = io.Copy(io.Discard, stdout)
	}
	if extractErr != nil {
		_ = cmd.Process.Kill()
	}
	_ = stdout.Close()
	waitErr := cmd.Wait()
	if extractErr != nil {
		return extractErr
	}
	if waitErr != nil {
		return fmt.Errorf("无法导出代码快照：%s", excerpt(redact(g.stderr.b.String(), g.token, g.auth)))
	}
	return nil
}

// Count while streaming, before each file is written. No archive-sized buffer,
// temporary tar file, or second filesystem walk is needed.
func extractSnapshot(ctx context.Context, r io.Reader, dest string, limits Limits) error {
	tr := tar.NewReader(r)
	var total int64
	var count int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("代码快照解包失败：%w", err)
		}
		name := path.Clean(h.Name)
		if !filepath.IsLocal(name) || strings.Contains(name, "\\") {
			return fmt.Errorf("代码快照含无效路径")
		}
		for _, component := range strings.Split(name, "/") {
			if strings.EqualFold(component, ".git") {
				return fmt.Errorf("代码快照不能包含 .git")
			}
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 || h.Size > limits.MaxSnapshotBytes-total {
				return fmt.Errorf("代码快照超过 %d MB 上限，请调高 files.code_projects.max_snapshot_mb，或改用只含所需目录的项目", limits.MaxSnapshotBytes>>20)
			}
			total += h.Size
			count++
			if count > limits.MaxSnapshotFiles {
				return fmt.Errorf("代码快照超过 %d 个文件上限，请调高 files.code_projects.max_snapshot_files", limits.MaxSnapshotFiles)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			mode := os.FileMode(0600)
			if h.Mode&0111 != 0 {
				mode = 0700
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(f, tr, h.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			// Symlinks, hardlinks and special files never enter a snapshot.
		}
	}
}
