package server

import (
	"context"
	"os"
	"time"
)

// defaultConfigPoll is how often Watch checks the config file for changes.
const defaultConfigPoll = time.Second

// fileSig is a cheap change-detection signature for the config file. A content
// edit changes the modtime (nanoseconds on the dev/CI filesystems), and most
// edits change the size too. Polling these two avoids a filesystem-watch
// dependency and is immune to editors' atomic-save rename dance, which breaks
// inode-level watches. modNano is stored as int64 so fileSig stays comparable
// with ==.
type fileSig struct {
	modNano int64
	size    int64
}

func statSig(path string) (fileSig, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileSig{}, false
	}
	return fileSig{modNano: fi.ModTime().UnixNano(), size: fi.Size()}, true
}

// Watch hot-reloads the engine whenever the on-disk config file changes, so
// edits made outside the web UI — a text editor, `git pull`, a config-management
// tool — take effect without restarting the server. It polls modtime+size on a
// short interval and calls reload() on change. Web-UI saves also bump the file,
// so a redundant (but idempotent) reload may follow one of those. Watch blocks
// until ctx is cancelled; it returns immediately when there is no file-backed
// config (env-only or empty), logging that the watcher is inactive.
func (s *Server) Watch(ctx context.Context) {
	path := s.watchPath()
	if path == "" {
		s.logf("无配置文件，热重载监听未启用（Web 编辑仍即时生效）")
		return
	}
	interval := s.pollInterval
	if interval <= 0 {
		interval = defaultConfigPoll
	}
	s.logf("正在监听配置变更: %s", path)

	last, _ := statSig(path)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sig, ok := statSig(path)
			if !ok || sig == last {
				continue // missing (mid atomic-save) or unchanged
			}
			last = sig
			if err := s.reload(); err != nil {
				s.logf("配置热重载失败，沿用旧配置: %v", err)
				continue
			}
			s.logf("配置已变更，热重载完成: %s", path)
		}
	}
}

// watchPath resolves the config file Watch should monitor: the explicit
// --config path when set, otherwise the file the running config was loaded from.
// Returns "" when the config came from env vars or no file exists.
func (s *Server) watchPath() string {
	if s.configPath != "" {
		return s.configPath
	}
	if p := s.engine().Config().SourcePath; p != "" && p != "(env)" {
		return p
	}
	return ""
}
