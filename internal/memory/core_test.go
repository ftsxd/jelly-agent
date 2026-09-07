package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCore(t *testing.T) *Core {
	t.Helper()
	c, err := NewCore(t.TempDir(), 0, 0, 0)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	return c
}

func TestRenderEmptyReturnsBase(t *testing.T) {
	c := newCore(t)
	if got := c.Render("BASE"); got != "BASE" {
		t.Fatalf("empty render = %q, want %q", got, "BASE")
	}
}

func TestRememberThenRender(t *testing.T) {
	c := newCore(t)
	if err := c.Remember(TargetMemory, "用户喜欢简洁回答"); err != nil {
		t.Fatalf("Remember memory: %v", err)
	}
	if err := c.Remember(TargetUser, "名字叫 Jelly"); err != nil {
		t.Fatalf("Remember user: %v", err)
	}
	out := c.Render("BASE")
	for _, want := range []string{"用户画像（USER.md）", "名字叫 Jelly", "长期记忆（MEMORY.md）", "用户喜欢简洁回答", "BASE"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n---\n%s", want, out)
		}
	}
	// User section must precede the memory section, which precedes base.
	if iu, im, ib := strings.Index(out, "用户画像"), strings.Index(out, "长期记忆"), strings.Index(out, "BASE"); !(iu < im && im < ib) {
		t.Errorf("section order wrong: user=%d memory=%d base=%d", iu, im, ib)
	}
}

func TestRememberDeduplicates(t *testing.T) {
	c := newCore(t)
	for i := 0; i < 3; i++ {
		if err := c.Remember(TargetMemory, "同一条事实"); err != nil {
			t.Fatalf("Remember: %v", err)
		}
	}
	mem, _ := c.Snapshot()
	if n := strings.Count(mem, "同一条事实"); n != 1 {
		t.Fatalf("fact stored %d times, want 1\n%s", n, mem)
	}
}

func TestRememberRejectsBlank(t *testing.T) {
	c := newCore(t)
	if err := c.Remember(TargetMemory, "   "); err == nil {
		t.Fatal("blank fact should error")
	}
}

func TestForget(t *testing.T) {
	c := newCore(t)
	mustRemember(t, c, "记住苹果", "记住香蕉", "记住橙子")

	n, err := c.Forget(TargetMemory, "香蕉")
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	mem, _ := c.Snapshot()
	if strings.Contains(mem, "香蕉") {
		t.Errorf("香蕉 not removed:\n%s", mem)
	}
	for _, keep := range []string{"苹果", "橙子"} {
		if !strings.Contains(mem, keep) {
			t.Errorf("removed too much, %q gone:\n%s", keep, mem)
		}
	}
}

func TestForgetCaseInsensitiveAndCount(t *testing.T) {
	c := newCore(t)
	mustRemember(t, c, "Likes Golang", "likes golang too", "likes rust")

	n, err := c.Forget(TargetMemory, "GOLANG")
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if n != 2 {
		t.Fatalf("removed %d, want 2", n)
	}
	mem, _ := c.Snapshot()
	if !strings.Contains(mem, "rust") || strings.Contains(strings.ToLower(mem), "golang") {
		t.Errorf("unexpected remainder:\n%s", mem)
	}
}

func TestForgetNoMatch(t *testing.T) {
	c := newCore(t)
	mustRemember(t, c, "唯一的事实")
	n, err := c.Forget(TargetMemory, "不存在")
	if err != nil || n != 0 {
		t.Fatalf("Forget no-match = (%d, %v), want (0, nil)", n, err)
	}
	if mem, _ := c.Snapshot(); !strings.Contains(mem, "唯一的事实") {
		t.Errorf("no-match forget altered file:\n%s", mem)
	}
}

func TestUnknownTargetErrors(t *testing.T) {
	c := newCore(t)
	if err := c.Remember("bogus", "x"); err == nil {
		t.Fatal("Remember to bogus target should error")
	}
	if _, err := c.Forget("bogus", "x"); err == nil {
		t.Fatal("Forget from bogus target should error")
	}
}

func TestBudgetTrimsOldestWithMarker(t *testing.T) {
	// Tight budget so only the most recent entries survive.
	c, err := NewCore(t.TempDir(), 12, 500, 0)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	for _, f := range []string{"最早的事实", "中间的事实", "最近的事实"} {
		if err := c.Remember(TargetMemory, f); err != nil {
			t.Fatalf("Remember: %v", err)
		}
	}
	out := c.budgeted(MemoryFile, 12)
	if !strings.Contains(out, "最近的事实") {
		t.Errorf("newest entry dropped:\n%s", out)
	}
	if !strings.Contains(out, "已省略") {
		t.Errorf("expected truncation marker:\n%s", out)
	}
	if strings.Contains(out, "最早的事实") {
		t.Errorf("oldest entry should be dropped:\n%s", out)
	}
}

func TestEstimateTokens(t *testing.T) {
	// CJK ~1 token/rune; ASCII ~4 chars/token.
	if got := estimateTokens("你好世界"); got != 4 {
		t.Errorf("estimateTokens(CJK) = %d, want 4", got)
	}
	if got := estimateTokens("abcdefgh"); got != 2 {
		t.Errorf("estimateTokens(ascii) = %d, want 2", got)
	}
}

func mustRemember(t *testing.T, c *Core, facts ...string) {
	t.Helper()
	for _, f := range facts {
		if err := c.Remember(TargetMemory, f); err != nil {
			t.Fatalf("Remember %q: %v", f, err)
		}
	}
}

// The environment block is injected every turn, ahead of the rest.
//
// Ahead because it is the ground the rest is reasoned against, and because it
// is the most stable of the three — the prompt cache rewards putting what
// changes least at the front, and MEMORY.md, which the agent itself rewrites,
// changes most.
func TestEnvironmentIsInjectedFirst(t *testing.T) {
	c, err := NewCore(t.TempDir(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(c.Dir(), name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(EnvironmentFile, "- 监控只有一套 n9e，数据源是 k8s Prometheus")
	write(UserFile, "- 用户偏好简洁的中文回答")
	write(MemoryFile, "- 上次排查过 payment-api 超时")

	got := c.Render("你是 jelly-agent。")
	for _, want := range []string{"ENVIRONMENT.md", "n9e", "USER.md", "MEMORY.md", "你是 jelly-agent。"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered prompt is missing %q:\n%s", want, got)
		}
	}
	env, usr, mem := strings.Index(got, "ENVIRONMENT.md"), strings.Index(got, "USER.md"), strings.Index(got, "MEMORY.md")
	if !(env < usr && usr < mem) {
		t.Errorf("顺序是 env=%d user=%d memory=%d，应当是环境最前、记忆最后", env, usr, mem)
	}
	if base := strings.Index(got, "你是 jelly-agent。"); base < mem {
		t.Errorf("静态指令排在了核心记忆之前 (%d < %d)", base, mem)
	}
}

// The agent cannot write the environment file.
//
// That is the whole reason it is a separate file rather than a section of
// MEMORY.md: it is what an operator asserts, and a model must not be able to
// overwrite or forget the ground truth it is reasoning against.
func TestTheAgentCannotWriteTheEnvironmentFile(t *testing.T) {
	c, err := NewCore(t.TempDir(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	const truth = "- 腾讯云 Redis 未接入本平台"
	if err := os.WriteFile(filepath.Join(c.Dir(), EnvironmentFile), []byte(truth), 0o644); err != nil {
		t.Fatal(err)
	}

	// Neither target reaches it, and there is no third target that does.
	for _, target := range []Target{TargetMemory, TargetUser, "", Target(EnvironmentFile), "environment", "env"} {
		_ = c.Remember(target, "- 腾讯云 Redis 已经接入了")
		_, _ = c.Forget(target, "腾讯云")
	}
	if got := c.Environment(); got != truth {
		t.Errorf("环境事实被改写成了 %q", got)
	}
}

// An empty or absent file contributes nothing, so every existing install
// behaves exactly as it did.
func TestNoEnvironmentFileChangesNothing(t *testing.T) {
	c, err := NewCore(t.TempDir(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Environment(); got != "" {
		t.Errorf("environment = %q, want empty", got)
	}
	if got := c.Render("基础指令"); got != "基础指令" {
		t.Errorf("render = %q, want the bare instruction", got)
	}
}

// The environment file is trimmed from the end, not the start.
//
// The direction is the whole point. MEMORY.md is notes where the newest matter
// most, so it keeps the tail. ENVIRONMENT.md opens with the data-source table
// — the ids a query cannot be made without — and closes with caveats. Trimming
// it the same way keeps the caveats and drops the table, which is the least
// useful half of the file with no sign that the rest existed.
func TestTheEnvironmentFileIsTrimmedFromTheEnd(t *testing.T) {
	c, err := NewCore(t.TempDir(), 0, 0, 12) // a budget small enough to bind
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Join([]string{
		"必须保留：ds_id=4 是 k8s 生产",
		"中间的行",
		"再一行",
		"最后一行：待确认的事情",
	}, "\n")
	if err := os.WriteFile(filepath.Join(c.Dir(), EnvironmentFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := c.Render("基础指令")
	if !strings.Contains(got, "ds_id=4") {
		t.Errorf("开头的数据源表被截掉了:\n%s", got)
	}
	if strings.Contains(got, "最后一行") {
		t.Errorf("预算没有生效，尾部也进来了:\n%s", got)
	}
	// And it says so, rather than looking like the whole file.
	if !strings.Contains(got, "未进入上下文") {
		t.Errorf("截断没有留下痕迹，读的人会以为这就是全部:\n%s", got)
	}

	// MEMORY.md keeps trimming the other way — the newest note survives.
	if err := os.WriteFile(filepath.Join(c.Dir(), MemoryFile),
		[]byte("- 最早的一条\n- 最新的一条"), 0o644); err != nil {
		t.Fatal(err)
	}
	tight, err := NewCore(c.Dir(), 6, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out := tight.Render("x"); !strings.Contains(out, "最新的一条") {
		t.Errorf("MEMORY.md 的裁剪方向被一起改掉了:\n%s", out)
	}
}
