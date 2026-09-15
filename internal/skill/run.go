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

// Visible resolves a skill for an agent: it exists, it is enabled, and this
// agent is allowed to see it. The three reasons a skill may be unavailable
// collapse into one answer on purpose — see RunScript.
//
// It is a method rather than inline in the use_skill tool so the rule can be
// tested without fabricating an ADK tool context.
func (s *Store) Visible(name string, allow Allowlist) (Skill, bool, error) {
	sk, ok, err := s.Get(name)
	if err != nil || !ok || !sk.Enabled || !allow.Permits(sk.Name) {
		return Skill{}, false, err
	}
	return sk, true, nil
}

// SkillMode resolves the sandbox mode one skill's scripts run under: the
// operator's policy, tightened by the skill's own `sandbox:` frontmatter if it
// declares one.
//
// The clamp only ever goes one way. A skill is content — imported from a zip,
// edited in the web form, in the limit written by the model itself — so letting
// it widen its own envelope would make the global policy advisory. Tightening is
// safe and worth supporting: a skill that only reads and reports should say so.
// An unreadable or malformed declaration falls back to the global policy.
func (s *Store) SkillMode(name string, pol sandbox.Policy) sandbox.Mode {
	global := pol.EffectiveMode()
	sk, ok, err := s.Get(name)
	if err != nil || !ok || sk.Sandbox == "" {
		return global
	}
	declared := sandbox.Mode(sk.Sandbox)
	if !declared.Valid() {
		return global
	}
	return declared.AtMost(global)
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
// the resource/isolation envelope is enforced by the sandbox package. A skill
// declaring its own `sandbox:` mode narrows pol for this run — see SkillMode.
//
// allow is the calling agent's skill allowlist, checked here rather than only at
// the tool boundary: this is the last point before a script actually executes,
// and it is reachable by a plain function call, which the tool closure is not.
func (s *Store) RunScript(ctx context.Context, name, script string, args []string, env map[string]string, pol sandbox.Policy, allow Allowlist) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("技能名非法")
	}
	if !allow.Permits(name) {
		// Same wording as a missing skill: the model gains nothing from
		// "exists but not yours" except a reason to retry.
		return "", fmt.Errorf("未找到该技能（或未启用）：%s", name)
	}
	pol.Mode = s.SkillMode(name, pol)
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

// modeList renders the valid sandbox modes for an error message.
func modeList() string {
	names := make([]string, 0, len(sandbox.Modes))
	for _, m := range sandbox.Modes {
		names = append(names, string(m))
	}
	return strings.Join(names, " / ")
}
