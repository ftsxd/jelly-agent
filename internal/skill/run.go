package skill

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxScriptOutput caps captured stdout+stderr so a chatty script can't flood the
// model context.
const maxScriptOutput = 8 << 10 // 8 KiB

// interpreters maps a script extension to the command that runs it. A file
// without a known extension is executed directly (must be +x with a shebang).
var interpreters = map[string]string{
	".sh":   "sh",
	".bash": "bash",
	".py":   "python3",
	".js":   "node",
}

// Scripts lists the runnable script files bundled with a directory-form skill
// (anything that isn't SKILL.md, top level only). Empty for flat-file skills.
func (s *Store) Scripts(name string) []string {
	if !ValidName(name) {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, name))
	if err != nil {
		return nil // flat skill or none
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.EqualFold(e.Name(), "SKILL.md") {
			continue
		}
		if _, ok := interpreters[strings.ToLower(filepath.Ext(e.Name()))]; ok || isExecutable(e) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func isExecutable(e os.DirEntry) bool {
	info, err := e.Info()
	return err == nil && info.Mode()&0o111 != 0
}

// RunScript runs script (a file inside the skill's directory) with env injected
// as environment variables, returning the combined, truncated output. The
// caller supplies a context with a timeout. Secrets in env reach only the child
// process — they are never returned except via what the script itself prints.
//
// Security: this executes code with the current user's privileges. It confines
// the path to the skill directory, applies the ctx timeout, and caps output,
// but is NOT a sandbox. It must only be reachable when scripts are enabled.
func (s *Store) RunScript(ctx context.Context, name, script string, args []string, env map[string]string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("技能名非法")
	}
	skillDir := filepath.Join(s.dir, name)

	// Resolve and confine the script path inside the skill directory.
	dest := filepath.Join(skillDir, filepath.FromSlash(script))
	rel, err := filepath.Rel(skillDir, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("脚本路径越界: %q", script)
	}
	if info, err := os.Stat(dest); err != nil || info.IsDir() {
		return "", fmt.Errorf("脚本不存在: %q", script)
	}

	var cmd *exec.Cmd
	if interp, ok := interpreters[strings.ToLower(filepath.Ext(dest))]; ok {
		cmd = exec.CommandContext(ctx, interp, append([]string{dest}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, dest, args...) // rely on +x / shebang
	}
	cmd.Dir = skillDir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// On ctx cancellation/timeout the process is killed; WaitDelay then force-
	// closes the output pipes shortly after, so a backgrounded grandchild that
	// inherited them can't keep Run blocked indefinitely.
	cmd.WaitDelay = 2 * time.Second

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	out := buf.String()
	if len(out) > maxScriptOutput {
		out = out[:maxScriptOutput] + "\n…（输出已截断）"
	}
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("脚本执行超时")
	}
	if runErr != nil {
		return out, fmt.Errorf("脚本退出异常: %w", runErr)
	}
	return out, nil
}
