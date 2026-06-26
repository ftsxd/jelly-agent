package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

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
		if _, ok := sandbox.Interpreters[strings.ToLower(filepath.Ext(e.Name()))]; ok || isExecutable(e) {
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

// RunScript runs script (a file inside the skill's directory) under the given
// sandbox policy, with env injected as variables, returning the combined,
// truncated output. The caller's ctx still bounds the run (the policy's timeout
// applies on top). Secrets in env reach only the child process — they are never
// returned except via what the script itself prints.
//
// Path safety is enforced here (the script must resolve inside the skill dir);
// the resource/isolation envelope is enforced by the sandbox package.
func (s *Store) RunScript(ctx context.Context, name, script string, args []string, env map[string]string, pol sandbox.Policy) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("技能名非法")
	}
	skillDir := filepath.Join(s.dir, name)

	// Resolve and confine the script path inside the skill directory.
	rel := filepath.FromSlash(script)
	dest := filepath.Join(skillDir, rel)
	clean, err := filepath.Rel(skillDir, dest)
	if err != nil || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("脚本路径越界: %q", script)
	}
	if info, err := os.Stat(dest); err != nil || info.IsDir() {
		return "", fmt.Errorf("脚本不存在: %q", script)
	}

	interp := sandbox.Interpreters[strings.ToLower(filepath.Ext(dest))] // "" → exec directly
	res, err := sandbox.Run(ctx, pol, sandbox.Spec{
		Dir:     skillDir,
		Interp:  interp,
		RelFile: clean,
		Args:    args,
		Env:     env,
	})
	if err != nil {
		return res.Output, fmt.Errorf("脚本无法启动: %w", err)
	}
	if res.TimedOut {
		return res.Output, fmt.Errorf("脚本执行超时")
	}
	if res.ExitCode != 0 {
		return res.Output, fmt.Errorf("脚本退出异常: exit status %d", res.ExitCode)
	}
	return res.Output, nil
}
