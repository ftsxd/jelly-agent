package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
	"github.com/jelly-agent/jelly-agent/internal/storage"
)

func journalStore(t *testing.T) (Journal, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = EnsureJournalSchema(db); err != nil {
		t.Fatal(err)
	}
	if err = EnsureApprovalSchema(db); err != nil {
		t.Fatal(err)
	}
	return Journal{DB: db, Root: filepath.Join(dir, "workspaces")}, path
}
func beginRun(t *testing.T, j Journal, daemon string) RunRecord {
	t.Helper()
	n, err := nonce()
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := j.Begin(t.Context(), "exec_"+n, "ops", "s", "read", "", daemon, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestJournalStartLeaseAndCleanupFence(t *testing.T) {
	j, _ := journalStore(t)
	r := beginRun(t, j, "daemon")
	if err := j.Starting(t.Context(), r, time.Minute); err == nil {
		t.Fatal("started without persisted container ID")
	}
	if err := j.Created(t.Context(), r, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := j.Starting(t.Context(), r, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := j.Starting(t.Context(), r, time.Minute); err == nil {
		t.Fatal("start replay")
	}
	r = beginRun(t, j, "daemon")
	j.DB.Exec(`UPDATE execution_runs SET deadline_ms=0 WHERE exec_id=?`, r.ExecID)
	if err := j.Created(t.Context(), r, strings.Repeat("b", 64)); err == nil {
		t.Fatal("expired intent accepted ID")
	}
	r = beginRun(t, j, "daemon")
	j.Created(t.Context(), r, strings.Repeat("c", 64))
	j.DB.Exec(`UPDATE execution_runs SET cleanup_state='cleaning' WHERE exec_id=?`, r.ExecID)
	if err := j.Starting(t.Context(), r, time.Minute); err == nil {
		t.Fatal("cleaner did not fence start")
	}
}
func TestJournalScopePublicationAndPrivateRoot(t *testing.T) {
	j, _ := journalStore(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var scopes []string
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			root, scope, err := j.openRoot(true)
			if err != nil {
				t.Error(err)
				return
			}
			root.Close()
			mu.Lock()
			scopes = append(scopes, scope)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(scopes) != 12 {
		t.Fatal("incomplete scope initialisation")
	}
	for _, s := range scopes {
		if s != scopes[0] {
			t.Fatal("scope publication race")
		}
	}
	foreign := t.TempDir()
	symlink := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(foreign, symlink); err != nil {
		t.Fatal(err)
	}
	j.Root = symlink
	if _, _, err := j.openRoot(true); err == nil {
		t.Fatal("symlink root accepted")
	}
	j.Root = foreign
	os.Chmod(foreign, 0755)
	if _, _, err := j.openRoot(true); err == nil {
		t.Fatal("public root accepted")
	}
}

func TestApprovalConsumptionAndIntentAreAtomic(t *testing.T) {
	j, _ := journalStore(t)
	store := Approvals{DB: j.DB}
	cfg := approvalConfig()
	req := approvalRequest()
	a := newApproval(t, store, cfg, req)
	if err := store.Resolve(t.Context(), a.ID, "s", "admin", true, cfg); err != nil {
		t.Fatal(err)
	}
	n, _ := nonce()
	record, err := j.Prepare("exec_"+n, "ops", "s", req.Profile, a.ID, "daemon", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Force the journal insert to fail. The grant must remain unconsumed.
	if err = insertIntent(t.Context(), j.DB, record); err != nil {
		t.Fatal(err)
	}
	if _, err = store.consumeIntent(WithApproval(t.Context(), a.ID), cfg, "ops", "s", "call", req, &record); err == nil {
		t.Fatal("duplicate intent committed")
	}
	got, err := store.Get(t.Context(), a.ID)
	if err != nil || got.State != "approved" || got.ExecID != "" {
		t.Fatal("partial consumption", got, err)
	}
	j.DB.Exec(`DELETE FROM execution_runs WHERE exec_id=?`, record.ExecID)
	if _, err = store.consumeIntent(WithApproval(t.Context(), a.ID), cfg, "ops", "s", "call", req, &record); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(t.Context(), a.ID)
	if err != nil || got.State != "consumed" || got.ExecID != record.ExecID {
		t.Fatal(got, err)
	}
	if _, err = j.Get(t.Context(), record.ExecID); err != nil {
		t.Fatal("consumed without journal", err)
	}
	if _, err = os.Stat(filepath.Join(j.Root, record.ExecID)); !os.IsNotExist(err) {
		t.Fatal("credentials workspace allocated before intent commit", err)
	}
	if err = store.Finish(t.Context(), a.ID, Observation{ExecID: record.ExecID, Executed: true, ExitCode: 0, Outcome: "succeeded", CleanupPending: true, Error: "cleanup pending"}); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(t.Context(), a.ID)
	if got.Outcome != "succeeded" {
		t.Fatal("cleanup overwrote business outcome", got)
	}
	j.DB.Exec(`UPDATE execution_approvals SET outcome='unknown' WHERE id=?`, a.ID)
	if err = store.Finish(t.Context(), a.ID, Observation{ExecID: record.ExecID, Executed: true, ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(t.Context(), a.ID)
	if got.Outcome != "unknown" {
		t.Fatal("late worker overwrote unknown", got)
	}
}

func dockerJournal(t *testing.T) (Journal, string, string) {
	t.Helper()
	if os.Getenv("JELLY_TEST_DOCKER") != "1" {
		t.Skip("set JELLY_TEST_DOCKER=1 for isolated Docker fault injection")
	}
	daemon, err := sandbox.DockerDaemonID(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = exec.Command("docker", "image", "inspect", "python:3.12-slim-bookworm").Run(); err != nil {
		t.Fatal("preinstalled test image required; never pull")
	}
	j, path := journalStore(t)
	return j, path, daemon
}
func createTestContainer(t *testing.T, r RunRecord, owner string) string {
	t.Helper()
	b, err := exec.Command("docker", "create", "--name", "jelly-"+r.ExecID, "--label", sandbox.LabelScope+"="+r.Scope, "--label", sandbox.LabelExec+"="+r.ExecID, "--label", sandbox.LabelOwner+"="+owner, "--network", "none", "--pull", "never", "python:3.12-slim-bookworm", "python", "-c", "import time; time.sleep(60)").Output()
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(b))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
	return id
}
func TestDockerJournalLateCreateAndForeignAuthority(t *testing.T) {
	j, _, daemon := dockerJournal(t)
	r := beginRun(t, j, daemon)
	j.DB.Exec(`UPDATE execution_runs SET deadline_ms=0 WHERE exec_id=?`, r.ExecID)
	if n, err := j.Recover(t.Context()); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	// A late workspace allocation can also follow the first absence check.
	if err := os.Mkdir(filepath.Join(j.Root, r.ExecID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(j.Root, r.ExecID, "credential"), []byte("test-only"), 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := j.Recover(t.Context()); err != nil || n != 1 {
		t.Fatal("late workspace", n, err)
	}
	// Create finishes after the first recovery found nothing. The retained
	// record must authorise cleanup without authorising a business restart.
	cid := createTestContainer(t, r, r.Owner)
	if n, err := j.Recover(t.Context()); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	got, err := j.Get(t.Context(), r.ExecID)
	if err != nil || got.State != "unknown" || got.CleanupState != "cleaned" || got.ContainerID != cid {
		t.Fatal(got, err)
	}
	if err = exec.Command("docker", "inspect", cid).Run(); err == nil {
		t.Fatal("late container survived")
	}
	r = beginRun(t, j, daemon)
	foreign := createTestContainer(t, r, strings.Repeat("f", 32))
	j.DB.Exec(`UPDATE execution_runs SET deadline_ms=0 WHERE exec_id=?`, r.ExecID)
	if _, err = j.Recover(t.Context()); err == nil {
		t.Fatal("foreign owner was accepted")
	}
	if err = exec.Command("docker", "inspect", foreign).Run(); err != nil {
		t.Fatal("foreign container removed")
	}
	if _, err = os.Stat(filepath.Join(j.Root, r.ExecID)); err != nil {
		t.Fatal("credentials removed without container clearance")
	}
}
func TestDockerJournalActiveLeaseDaemonAndSymlink(t *testing.T) {
	j, _, daemon := dockerJournal(t)
	r := beginRun(t, j, daemon)
	cid := createTestContainer(t, r, r.Owner)
	if n, err := j.Recover(t.Context()); err != nil || n != 0 {
		t.Fatal("active execution recovered", n, err)
	}
	if err := exec.Command("docker", "inspect", cid).Run(); err != nil {
		t.Fatal(err)
	}
	j.DB.Exec(`UPDATE execution_runs SET deadline_ms=0,daemon='different-daemon' WHERE exec_id=?`, r.ExecID)
	if _, err := j.Recover(t.Context()); err == nil {
		t.Fatal("changed daemon accepted")
	}
	if err := exec.Command("docker", "inspect", cid).Run(); err != nil {
		t.Fatal("removed on wrong daemon")
	}
	j.DB.Exec(`UPDATE execution_runs SET daemon=? WHERE exec_id=?`, daemon, r.ExecID)
	work := filepath.Join(j.Root, r.ExecID)
	os.RemoveAll(work)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("private"), 0600)
	os.Symlink(outside, work)
	if _, err := j.Recover(t.Context()); err == nil {
		t.Fatal("workspace symlink accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("outside directory changed")
	}
}

func TestDockerRuntimeResultsAndCleanup(t *testing.T) {
	j, _, _ := dockerJournal(t)
	t.Setenv("JELLY_TEST_PRIVATE_KUBE", "apiVersion: v1\nclusters: []\nusers: []\n")
	for _, trial := range []struct {
		name, script  string
		exit, timeout int
	}{
		{"success", "print('complete')", 0, 10},
		{"failure", "import sys; print('diagnostic'); sys.exit(7)", 7, 10},
		{"restrictive-workspace", "import os; os.chmod('/work/.kube',0); os.chmod('/work',0); print('complete')", 0, 10},
		{"timeout", "import time; time.sleep(30)", -1, 1},
	} {
		t.Run(trial.name, func(t *testing.T) {
			cfg := crashConfig()
			cfg.TimeoutSec = trial.timeout
			cfg.Profiles[0].Rules[0].Decision = Allow
			cfg.Profiles[0].KubeconfigEnv = "JELLY_TEST_PRIVATE_KUBE"
			rt := Runtime{Config: cfg, Journal: &j}
			out := rt.Execute(t.Context(), "ops", "s", Request{Command: "python -c \"" + strings.ReplaceAll(trial.script, "\"", "\\\"") + "\"", Profile: "read", Purpose: "isolated result test"})
			if !out.Executed || out.CleanupPending {
				t.Fatal(out)
			}
			if trial.name == "timeout" {
				if !out.TimedOut || out.Error == "" {
					t.Fatal(out)
				}
			} else if out.ExitCode != trial.exit {
				t.Fatal(out)
			}
			row, err := j.Get(t.Context(), out.ExecID)
			if err != nil || row.CleanupState != "cleaned" {
				t.Fatal(row, err)
			}
			if _, err = os.Stat(filepath.Join(j.Root, out.ExecID)); !os.IsNotExist(err) {
				t.Fatal("credentials remain", err)
			}
			if _, absent, err := sandbox.InspectManaged(t.Context(), identity(row)); err != nil || !absent {
				t.Fatal("container remains", err)
			}
		})
	}
}

func crashConfig() Config {
	c := testConfig()
	c.Backend = "docker"
	c.Image = "python:3.12-slim-bookworm"
	c.TimeoutSec = 60
	c.Profiles[0].WriteApproval = true
	c.Profiles[0].Rules = []Rule{{Name: "test-python", Pattern: []string{"python", "-c"}, Decision: Prompt}}
	return c
}

const crashScript = `import os,subprocess,time; subprocess.Popen(['python','-c','import os,time; open("/work/detached","w").write(str(os.getsid(0))); time.sleep(60)'],start_new_session=True); open('/work/main','w').write(str(os.getsid(0))); time.sleep(60)`

func TestDockerJournalSIGKILLRecovery(t *testing.T) {
	if path := os.Getenv("JELLY_RECOVERY_HELPER_DB"); path != "" {
		db, err := storage.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		j := Journal{DB: db, Root: filepath.Join(filepath.Dir(path), "workspaces")}
		store := Approvals{DB: db}
		cfg := crashConfig()
		r := Runtime{Config: cfg, Journal: &j, Approvals: &store}
		req := Request{Command: "python -c \"" + strings.ReplaceAll(crashScript, "\"", "\\\"") + "\"", Profile: "read", Purpose: "isolated crash test"}
		a, err := store.Create(context.Background(), cfg, "ops", "s", "round", "call", req)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Resolve(context.Background(), a.ID, "s", "test-admin", true, cfg); err != nil {
			t.Fatal(err)
		}
		out := r.ExecuteApproved(WithApproval(context.Background(), a.ID), "ops", "s", "call", req)
		if !out.Executed {
			t.Fatal(out)
		}
		return
	}
	j, path, _ := dockerJournal(t)
	// Double quotes delimit the literal script; quoting is rebuilt by Parse.
	// The helper receives only a private DB path, no host credentials.
	cmd := exec.Command(os.Args[0], "-test.run=^TestDockerJournalSIGKILLRecovery$")
	cmd.Env = append(os.Environ(), "JELLY_RECOVERY_HELPER_DB="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait()
	}()
	var record RunRecord
	registeredCleanup := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := j.DB.Query(`SELECT ` + runColumns + ` FROM execution_runs`)
		if err != nil {
			t.Fatal(err)
		}
		if rows.Next() {
			record, err = scanRun(rows)
		}
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if record.ContainerID != "" && !registeredCleanup {
			containerID := record.ContainerID
			t.Cleanup(func() { exec.Command("docker", "rm", "-f", containerID).Run() })
			registeredCleanup = true
		}
		dir := filepath.Join(j.Root, record.ExecID)
		main, e1 := os.ReadFile(filepath.Join(dir, "main"))
		detached, e2 := os.ReadFile(filepath.Join(dir, "detached"))
		if e1 == nil && e2 == nil {
			if string(main) == string(detached) {
				t.Fatal("child did not escape process group")
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if record.ContainerID == "" {
		t.Fatal("container never started")
	}
	if _, err := os.Stat(filepath.Join(j.Root, record.ExecID, "detached")); err != nil {
		t.Fatal("detached child never became ready", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	cmd.Process = nil
	info, absent, err := sandbox.InspectManaged(t.Context(), identity(record))
	if err != nil || absent || !info.Running {
		t.Fatal("fixture did not survive service SIGKILL", info, absent, err)
	}
	// Expire the bounded lease to simulate the independently supervised janitor.
	j.DB.Exec(`UPDATE execution_runs SET deadline_ms=0 WHERE exec_id=?`, record.ExecID)
	if n, err := j.Recover(t.Context()); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	recovered, err := j.Get(t.Context(), record.ExecID)
	if err != nil || recovered.State != "unknown" || recovered.CleanupState != "cleaned" {
		t.Fatal(recovered, err)
	}
	if _, err := os.Stat(filepath.Join(j.Root, record.ExecID)); !os.IsNotExist(err) {
		t.Fatal("workspace remains", err)
	}
	if _, absent, err := sandbox.InspectManaged(t.Context(), identity(record)); err != nil || !absent {
		t.Fatal("container remains", err)
	}
	store := Approvals{DB: j.DB}
	a, err := store.Get(t.Context(), record.ApprovalID)
	if err != nil || a.State != "consumed" || a.Outcome != "unknown" || a.ExecID != record.ExecID {
		t.Fatal("crashed approval", a, err)
	}
	rt := Runtime{Config: crashConfig(), Approvals: &store, Journal: &j}
	out := rt.ExecuteApproved(WithApproval(t.Context(), a.ID), "ops", "s", "call", a.Request)
	if out.Executed || out.Decision != Forbidden {
		t.Fatal("crashed write replayed", out)
	}
}
