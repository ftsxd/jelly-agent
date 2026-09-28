package sandbox

import (
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestStrictRefusesFallbackBeforeCommand(t *testing.T) {
	for _, p := range []Policy{{Strict: true, Backend: "native", Mode: ModeWorkspace}, {Strict: true, Backend: "os", Mode: ModeFull}} {
		if res, err := Run(t.Context(), p, Spec{Dir: t.TempDir(), Argv: []string{"definitely-never-run"}}); err == nil || res.ExitCode != -1 {
			t.Fatalf("strict fallback %+v %v", res, err)
		}
	}
}

func TestLiteralArgvAndDockerHardening(t *testing.T) {
	s := Spec{Dir: "/work", Argv: []string{"tccli", "cls", "DescribeTopics"}, Env: map[string]string{"TOKEN": "private-test-value"}}
	if got := targetArgv(s); !reflect.DeepEqual(got, s.Argv) {
		t.Fatal(got)
	}
	args := strings.Join(dockerArgs(Policy{Strict: true, Mode: ModeWorkspace, Image: "diagnostic", MaxProcs: 8, MemoryMB: 64}, s), " ")
	for _, want := range []string{"--network none", "--read-only", "--cap-drop ALL", "--security-opt no-new-privileges", "--pull never", "diagnostic tccli cls DescribeTopics"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %s: %s", want, args)
		}
	}
	if strings.Contains(args, "private-test-value") || !strings.Contains(args, "--env TOKEN") {
		t.Fatal("injected credential appeared in Docker process arguments", args)
	}
}

func TestCaptureSeparatesStreamsAndDrainsBeyondCap(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	res, err := capture(exec.Command("sh", "-c", "printf stdout; printf stderr >&2; i=0; while [ $i -lt 3000 ]; do printf x; i=$((i+1)); done; exit 7"), 32)
	if err != nil || res.ExitCode != 7 || !res.Truncated || !strings.HasPrefix(res.Stdout, "stdout") || res.Stderr != "stderr" || len(res.Output) > 32 || len(res.Stdout) > 32 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestBoundedOutputAcknowledgesDiscardedBytes(t *testing.T) {
	var mu sync.Mutex
	b := boundedOutput{max: 4, mu: &mu}
	for i := 0; i < 200; i++ {
		if n, err := b.Write([]byte("abcdef")); n != 6 || err != nil {
			t.Fatal(n, err)
		}
	}
	if b.buf.String() != "abcd" || !b.truncated {
		t.Fatal(b)
	}
}
