package codeproject

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/rand"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type gitResponseCounter struct {
	http.ResponseWriter
	bytes *atomic.Int64
}

func (w gitResponseCounter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes.Add(int64(n))
	return n, err
}

// Exercise real smart HTTPS transport without relaxing production's protocol
// whitelist. Only this test's Git wrapper trusts the fixture server's CA.
func TestIncrementalSyncHTTPS(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(realGit, append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	source := t.TempDir()
	run(root, "init", "--bare", remote)
	run(source, "init", "-b", "main")
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(message string) string {
		t.Helper()
		run(source, "add", ".")
		run(source, "commit", "-qm", message)
		run(source, "push", "--force", remote, "HEAD:refs/heads/main")
		return run(source, "rev-parse", "HEAD")
	}
	write("main.go", "package main\n")
	write("removed.go", "package old\n")
	write(".gitattributes", "hidden.go export-ignore\nversion.txt export-subst\n")
	write("hidden.go", "package hidden\n")
	write("version.txt", "$Format:%H$\n")
	// An unchanged, incompressible blob makes accidental full downloads visible.
	large := make([]byte, 128<<10)
	_, _ = rand.New(rand.NewSource(1)).Read(large)
	write("unchanged.bin", string(large))
	if err := os.Symlink("/etc/passwd", filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	firstRevision := commit("initial")
	backend := &cgi.Handler{
		Path: filepath.Join(run(root, "--exec-path"), "git-http-backend"),
		Dir:  root,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull},
	}
	const token = "fixture-token-not-for-disk"
	var reject atomic.Bool
	var transferred atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if reject.Load() || !ok || user != "oauth2" || password != token {
			http.Error(w, "authentication failed", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(gitResponseCounter{w, &transferred}, r)
	}))
	defer server.Close()
	bin := t.TempDir()
	ca := filepath.Join(bin, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	log := filepath.Join(bin, "commands")
	wrapper := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + quote(log) + "\nexec " + quote(realGit) + " -c " + quote("http.sslCAInfo="+ca) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := Open(t.TempDir())
	p := testProject()
	p.URL = server.URL + "/remote.git"
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetToken(p.ID, token); err != nil {
		t.Fatal(err)
	}
	syncProject := func() Project {
		t.Helper()
		if err := s.Sync(context.Background(), p.ID); err != nil {
			t.Fatal(err)
		}
		ps, err := s.List()
		if err != nil || len(ps) != 1 || ps[0].SyncState != SyncSucceeded {
			t.Fatalf("sync result: %+v %v", ps, err)
		}
		return ps[0]
	}
	assertFile := func(p Project, name, want string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(s.dir, p.Snapshot, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: got %q, %v; want %q", name, b, err, want)
		}
	}
	first := syncProject()
	firstBytes := transferred.Swap(0)
	if first.Revision != firstRevision {
		t.Fatalf("wrong revision: %s", first.Revision)
	}
	assertFile(first, "hidden.go", "package hidden\n")
	assertFile(first, "version.txt", "$Format:%H$\n")
	for _, name := range []string{"escape", ".git"} {
		if _, err := os.Lstat(filepath.Join(s.dir, first.Snapshot, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("snapshot exposes %s", name)
		}
	}
	cache := s.gitCachePath(p.ID)
	sentinel := filepath.Join(cache, "reuse-sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	unchanged := syncProject()
	unchangedBytes := transferred.Swap(0)
	if unchanged.Snapshot != first.Snapshot {
		t.Fatal("unchanged revision rebuilt the snapshot")
	}
	commands, _ := os.ReadFile(log)
	if strings.Count(string(commands), " archive ") != 1 || strings.Count(string(commands), " init ") != 1 || strings.Count(string(commands), " fetch ") != 2 {
		t.Fatalf("unchanged sync did not reuse the cache and snapshot:\n%s", commands)
	}
	write("main.go", "package updated\n")
	write("new.go", "package newfile\n")
	if err := os.Remove(filepath.Join(source, "removed.go")); err != nil {
		t.Fatal(err)
	}
	secondRevision := commit("update")
	second := syncProject()
	updateBytes := transferred.Swap(0)
	if updateBytes*5 >= firstBytes || unchangedBytes*5 >= firstBytes {
		t.Fatalf("full transfer was repeated: first=%d, unchanged=%d, update=%d", firstBytes, unchangedBytes, updateBytes)
	}
	t.Logf("HTTPS response bytes: initial=%d, unchanged=%d, update=%d", firstBytes, unchangedBytes, updateBytes)
	if second.Revision != secondRevision || second.Snapshot == first.Snapshot {
		t.Fatalf("update not published: %+v", second)
	}
	assertFile(second, "main.go", "package updated\n")
	assertFile(second, "new.go", "package newfile\n")
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("changed revision recreated the cache", err)
	}
	for _, name := range []string{filepath.Join(s.dir, first.Snapshot), filepath.Join(s.dir, second.Snapshot, "removed.go")} {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale path retained: %s", name)
		}
	}
	// A rejected fetch must not export the previous local ref as a new success.
	reject.Store(true)
	if err := s.Sync(context.Background(), p.ID); err == nil {
		t.Fatal("rejected fetch succeeded")
	}
	ps, _ := s.List()
	if ps[0].Snapshot != second.Snapshot || ps[0].Revision != secondRevision || ps[0].SyncState != SyncFailed {
		t.Fatalf("failed fetch damaged snapshot: %+v", ps[0])
	}
	assertFile(ps[0], "main.go", "package updated\n")
	reject.Store(false)
	// A successful fetch followed by an export limit failure must keep the
	// published snapshot. Retrying should use the already-updated cache.
	write("main.go", "package third\n")
	thirdRevision := commit("third")
	s.SetLimits(Limits{MaxSnapshotFiles: 1})
	if err := s.Sync(context.Background(), p.ID); err == nil {
		t.Fatal("over-limit snapshot was published")
	}
	ps, _ = s.List()
	if ps[0].Snapshot != second.Snapshot || ps[0].Revision != secondRevision {
		t.Fatal("export failure replaced the previous snapshot")
	}
	s.SetLimits(DefaultLimits)
	third := syncProject()
	if third.Revision != thirdRevision {
		t.Fatal("retry did not publish the fetched revision")
	}
	assertFile(third, "main.go", "package third\n")
	// Force the maintenance threshold and verify it runs without losing the
	// current shallow tip or rebuilding its unchanged snapshot.
	marker := filepath.Join(cache, "codeproject.json")
	state, _ := json.Marshal(gitCacheState{URL: p.URL, Branch: p.Branch, Fetches: 31})
	if err := os.WriteFile(marker, state, 0600); err != nil {
		t.Fatal(err)
	}
	maintained := syncProject()
	if maintained.Snapshot != third.Snapshot {
		t.Fatal("maintenance rebuilt an unchanged snapshot")
	}
	state, err = os.ReadFile(marker)
	var cacheState gitCacheState
	if err != nil || json.Unmarshal(state, &cacheState) != nil || cacheState.Fetches != 0 {
		t.Fatalf("cache maintenance did not complete: %s, %v", state, err)
	}
	// Housekeeping that cannot finish in its budget must not be retried on every
	// sync afterwards: that turns maintenance into a fixed tax on the thing the
	// cache exists to make fast.
	gcBudget = time.Nanosecond
	state, _ = json.Marshal(gitCacheState{URL: p.URL, Branch: p.Branch, Fetches: 31})
	if err := os.WriteFile(marker, state, 0600); err != nil {
		t.Fatal(err)
	}
	syncProject()
	state, err = os.ReadFile(marker)
	if err != nil || json.Unmarshal(state, &cacheState) != nil || cacheState.Fetches != 0 {
		t.Fatalf("回收失败后计数没有退回，之后每次同步都要再等一遍: %s, %v", state, err)
	}
	gcBudget = 30 * time.Second
	// A force-push backwards is a valid update despite the shallow boundary.
	run(source, "reset", "--hard", firstRevision)
	run(source, "push", "--force", remote, "HEAD:refs/heads/main")
	rewound := syncProject()
	if rewound.Revision != firstRevision {
		t.Fatal("force-push did not update the snapshot")
	}
	assertFile(rewound, "main.go", "package main\n")
	// Missing snapshots are rebuilt even when the revision has not changed.
	if err := os.RemoveAll(filepath.Join(s.dir, rewound.Snapshot)); err != nil {
		t.Fatal(err)
	}
	rebuilt := syncProject()
	assertFile(rebuilt, "main.go", "package main\n")
	// A malformed cache is replaceable; it must not require deleting a valid
	// code snapshot or silently failing every subsequent synchronization.
	if err := os.Remove(filepath.Join(cache, "HEAD")); err != nil {
		t.Fatal(err)
	}
	repaired := syncProject()
	if repaired.Snapshot != rebuilt.Snapshot {
		t.Fatal("cache repair discarded an unchanged snapshot")
	}
	// Neither the cache nor its metadata can persist the injected auth header.
	if err := filepath.WalkDir(cache, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(name)
		if bytes.Contains(b, []byte(token)) || bytes.Contains(b, []byte("Authorization:")) {
			t.Fatalf("cache contains credential: %s", name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A branch change removes the old cache and old snapshot.
	run(source, "push", remote, "HEAD:refs/heads/develop")
	p.Branch = "develop"
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("branch change retained cache")
	}
	syncProject()
	if err := s.Delete(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("delete retained cache")
	}
}

func TestExtractSnapshotLimitsAndPaths(t *testing.T) {
	archive := func(headers ...*tar.Header) []byte {
		t.Helper()
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		for _, h := range headers {
			if err := w.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if h.Typeflag == tar.TypeReg {
				if _, err := w.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	file := func(name string, size int64) *tar.Header {
		return &tar.Header{Name: name, Typeflag: tar.TypeReg, Size: size, Mode: 0644}
	}
	for name, tt := range map[string]struct {
		data   []byte
		limits Limits
	}{
		"size":      {archive(file("large", 11)), Limits{MaxSnapshotBytes: 10, MaxSnapshotFiles: 2}},
		"total":     {archive(file("one", 6), file("two", 6)), Limits{MaxSnapshotBytes: 10, MaxSnapshotFiles: 2}},
		"count":     {archive(file("one", 0), file("two", 0)), Limits{MaxSnapshotBytes: 10, MaxSnapshotFiles: 1}},
		"traversal": {archive(file("../escape", 1)), DefaultLimits},
		"absolute":  {archive(file("/escape", 1)), DefaultLimits},
		"git":       {archive(file("a/.git/config", 1)), DefaultLimits},
		"git-case":  {archive(file(".GIT/config", 1)), DefaultLimits},
		"backslash": {archive(file(`a\escape`, 1)), DefaultLimits},
		"duplicate": {archive(file("same", 1), file("same", 1)), DefaultLimits},
		"truncated": {archive(file("broken", 100))[:550], DefaultLimits},
	} {
		t.Run(name, func(t *testing.T) {
			if err := extractSnapshot(context.Background(), bytes.NewReader(tt.data), t.TempDir(), tt.limits); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
	dest := t.TempDir()
	data := archive(
		file("nested/file", 4),
		&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		&tar.Header{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "nested/file"},
		&tar.Header{Name: "pipe", Typeflag: tar.TypeFifo},
	)
	if err := extractSnapshot(context.Background(), bytes.NewReader(data), dest, Limits{MaxSnapshotBytes: 4, MaxSnapshotFiles: 1}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link", "hardlink", "pipe"} {
		if _, err := os.Lstat(filepath.Join(dest, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("special entry extracted: %s", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := extractSnapshot(ctx, bytes.NewReader(data), t.TempDir(), DefaultLimits); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestReleaseDoesNotClearNewSyncReservation(t *testing.T) {
	s := Open(t.TempDir())
	s.busy["project"] = "new-task"
	s.release("project", "old-task")
	if got := s.busy["project"]; got != "new-task" {
		t.Fatalf("old sync cleared new reservation: %q", got)
	}
}
