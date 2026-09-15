package codeproject

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A pull that was in flight when the process stopped leaves "running" on disk
// with no goroutine behind it. Without recovery the project shows as syncing
// forever and every mutation stays blocked.
func TestRestartMarksAnInFlightSyncInterrupted(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	p := testProject()
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.read()
	ps[0].SyncState = SyncRunning
	ps[0].SyncTaskID = "sync-abandoned"
	if err := s.write(ps); err != nil {
		t.Fatal(err)
	}

	// Simulate a restart: drop the cached Store so Open rebuilds it.
	stores.Delete(mustAbs(dir))
	restarted := Open(dir)
	got, err := restarted.List()
	if err != nil {
		t.Fatal(err)
	}
	if got[0].SyncState != SyncInterrupted || got[0].SyncTaskID != "" {
		t.Fatalf("abandoned sync not recovered: %+v", got[0])
	}
	if got[0].Syncing {
		t.Fatal("recovered project still reports as syncing")
	}
	if got[0].LastError == "" {
		t.Fatal("recovered project gives the operator no reason")
	}
	// Recovery must not block a retry.
	if err := restarted.Save(p); err != nil {
		t.Fatal("retry blocked after recovery:", err)
	}
}

// Pressing sync twice must never start a second clone of the same repository.
func TestStartSyncIsIdempotentWhileRunning(t *testing.T) {
	s := Open(t.TempDir())
	p := testProject()
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.busy[p.ID] = "sync-inflight"
	s.mu.Unlock()

	task, err := s.StartSync(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task != "sync-inflight" {
		t.Fatalf("duplicate sync started a new task: %q", task)
	}
	if err := s.Sync(context.Background(), p.ID); err != ErrBusy {
		t.Fatalf("synchronous path ignored the running sync: %v", err)
	}
}

func TestStartSyncRecordsTheTaskAndReturnsImmediately(t *testing.T) {
	s := Open(t.TempDir())
	// A clone that cannot possibly succeed: what matters here is that the call
	// returns before the pull does, and that the state is on disk meanwhile.
	s.SetLimits(Limits{SyncTimeout: 5 * time.Second})
	p := testProject()
	p.URL = "https://127.0.0.1:1/nothing.git"
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	task, err := s.StartSync(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("StartSync blocked on the pull for %s", elapsed)
	}
	if task == "" {
		t.Fatal("no task id returned")
	}
	ps, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if !ps[0].Syncing || ps[0].SyncTaskID != task {
		t.Fatalf("task not recorded: %+v", ps[0])
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ps, _ = s.List()
		if ps[0].SyncState == SyncFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ps[0].SyncState != SyncFailed || ps[0].LastError == "" {
		t.Fatalf("failed pull not reported: %+v", ps[0])
	}
	if ps[0].SyncTaskID != "" || ps[0].Syncing {
		t.Fatalf("finished task not cleared: %+v", ps[0])
	}
}

func TestCheckDirsReportsMissingAndNonDirectories(t *testing.T) {
	snapshot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(snapshot, "services", "order"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "common"), []byte("not a dir"), 0600); err != nil {
		t.Fatal(err)
	}
	p := Project{RootPath: "services/order", ReferencePaths: []string{"common", "ad"}}
	issues := checkDirs(snapshot, p)
	if len(issues) != 2 {
		t.Fatalf("expected the file and the missing directory, got %v", issues)
	}

	// A whole-repo project has nothing to check.
	if len(checkDirs(snapshot, Project{})) != 0 {
		t.Fatal("whole-repo project reported directory issues")
	}
}

func mustAbs(p string) string {
	abs, _ := filepath.Abs(p)
	return abs
}
