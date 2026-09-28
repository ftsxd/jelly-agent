package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fake daemon reports only the fields the production code asks for. No
// Docker socket, container, cloud API, or injected credential is used here.
func fakeManagedDocker(t *testing.T, started bool, exit int) (Spec, string, *[]string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test Docker shim requires /bin/sh")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "create-args")
	inspectFile := filepath.Join(dir, "inspect.json")
	id := ContainerIdentity{
		Name:   "jelly-exec_dddddddddddddddddddddddddddddddd",
		ID:     strings.Repeat("a", 64),
		ExecID: "exec_dddddddddddddddddddddddddddddddd",
		Owner:  strings.Repeat("c", 32),
		Scope:  strings.Repeat("b", 32),
		Daemon: "managed-docker-test-daemon",
	}
	info := ContainerInfo{ID: id.ID, Name: "/" + id.Name, Exit: exit,
		Labels:  map[string]string{LabelScope: id.Scope, LabelExec: id.ExecID, LabelOwner: id.Owner},
		Started: "0001-01-01T00:00:00Z"}
	if started {
		info.Started = "2026-09-28T00:00:00Z"
	}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inspectFile, b, 0600); err != nil {
		t.Fatal(err)
	}
	shim := `#!/bin/sh
case "$1" in
  info) printf '%s\n' managed-docker-test-daemon ;;
  create)
    printf '%s\n' "$@" > "$JELLY_TEST_DOCKER_ARGS"
    printf '%s\n' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
  start)
    if [ "$JELLY_TEST_DOCKER_STARTED" = no ]; then
      printf '%s\n' 'daemon refused start' >&2
      exit 125
    fi
    printf '%s' 'business-output'
    exit "$JELLY_TEST_DOCKER_EXIT" ;;
  inspect) cat "$JELLY_TEST_DOCKER_INSPECT" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("JELLY_TEST_DOCKER_ARGS", argsFile)
	t.Setenv("JELLY_TEST_DOCKER_INSPECT", inspectFile)
	t.Setenv("JELLY_TEST_DOCKER_STARTED", "no")
	if started {
		t.Setenv("JELLY_TEST_DOCKER_STARTED", "yes")
	}
	t.Setenv("JELLY_TEST_DOCKER_EXIT", strconv.Itoa(exit))
	var stages []string
	id.ID = "" // create must publish the full ID before start is authorised.
	spec := Spec{Dir: dir, Argv: []string{"business-command"}, Managed: &ManagedContainer{
		Identity: id,
		Created: func(container string) error {
			if container != strings.Repeat("a", 64) {
				t.Errorf("unexpected created container ID: %q", container)
			}
			stages = append(stages, "created")
			return nil
		},
		Starting: func() error { stages = append(stages, "starting"); return nil },
	}}
	return spec, argsFile, &stages
}

func TestManagedDockerStartRefusedIsNotSuccess(t *testing.T) {
	spec, _, stages := fakeManagedDocker(t, false, 0)
	res, err := runManagedDocker(t.Context(), Policy{Strict: true, Backend: "docker", Image: "test-image", Timeout: 5 * time.Second}.withDefaults(), spec)
	if err == nil || res.Started || res.ExitCode == 0 {
		t.Fatalf("unstarted container was reported as successful: %+v, %v", res, err)
	}
	if !strings.Contains(res.Stderr, "daemon refused start") {
		t.Fatalf("missing start failure evidence: %+v", res)
	}
	if !reflect.DeepEqual(*stages, []string{"created", "starting"}) {
		t.Fatal(*stages)
	}
}

func TestManagedDockerBusinessExitCode(t *testing.T) {
	spec, _, stages := fakeManagedDocker(t, true, 7)
	res, err := runManagedDocker(t.Context(), Policy{Strict: true, Backend: "docker", Image: "test-image", Timeout: 5 * time.Second}.withDefaults(), spec)
	if err != nil || !res.Started || res.ExitCode != 7 || res.Stdout != "business-output" {
		t.Fatalf("business failure was not preserved: %+v, %v", res, err)
	}
	if !reflect.DeepEqual(*stages, []string{"created", "starting"}) {
		t.Fatal(*stages)
	}
}

func TestManagedDockerPreservesBusinessAutoRemoveArgument(t *testing.T) {
	spec, argsFile, _ := fakeManagedDocker(t, true, 0)
	spec.Argv = []string{"business-command", "--rm", "opaque"}
	res, err := runManagedDocker(t.Context(), Policy{Strict: true, Backend: "docker", Image: "test-image", Timeout: 5 * time.Second}.withDefaults(), spec)
	if err != nil || !res.Started || res.ExitCode != 0 {
		t.Fatalf("managed execution failed: %+v, %v", res, err)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	imageAt := -1
	for i, arg := range args {
		if arg == "test-image" {
			imageAt = i
			break
		}
		if arg == "--rm" {
			t.Fatalf("managed create accidentally enables daemon auto-removal: %q", args)
		}
	}
	if imageAt < 0 || !reflect.DeepEqual(args[imageAt+1:], spec.Argv) {
		t.Fatalf("business argv changed: %q", args)
	}
}
