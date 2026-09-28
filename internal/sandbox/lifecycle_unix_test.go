//go:build unix

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSuccessfulLeaderTerminatesBackgroundGroup(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	dir := t.TempDir()
	code := `import os,time
pid=os.fork()
if pid==0:
 fd=os.open('/dev/null',os.O_RDWR)
 os.dup2(fd,1); os.dup2(fd,2)
 time.sleep(0.3)
 open('orphan-marker','w').write('escaped')
 os._exit(0)
print(pid,flush=True)`
	res, err := Run(t.Context(), Policy{Backend: "native", Mode: ModeWorkspace, Timeout: 2 * time.Second}, Spec{Dir: dir, Argv: []string{"python3", "-c", code}})
	if err != nil || !res.Started || res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		t.Fatalf("parent failed: %+v %v", res, err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "orphan-marker")); !os.IsNotExist(err) {
		t.Fatalf("background child survived normal completion: %v", err)
	}
}

func TestStartedCommandWithIncompleteOutputIsNotStartFailure(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	cmd := exec.Command("sh", "-c", "sleep 1 & printf started")
	configureProc(cmd)
	defer killProc(cmd)
	cmd.WaitDelay = 20 * time.Millisecond
	res, err := capture(cmd, 1024)
	if !errors.Is(err, exec.ErrWaitDelay) || !res.Started || !res.Truncated || res.Stdout != "started" {
		t.Fatalf("started command lost its output failure metadata: %+v %v", res, err)
	}
}
