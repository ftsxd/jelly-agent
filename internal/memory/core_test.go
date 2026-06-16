package memory

import (
	"strings"
	"testing"
)

func newCore(t *testing.T) *Core {
	t.Helper()
	c, err := NewCore(t.TempDir(), 0, 0)
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
	c, err := NewCore(t.TempDir(), 12, 500)
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
