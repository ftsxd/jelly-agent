// Package memory implements jelly-agent's L1 Core Memory (Hermes-style): two
// small markdown files — MEMORY.md (the agent's long-term notes) and USER.md
// (the user profile) — injected into the agent's system instruction every turn
// at zero latency, with no tool round-trip. The agent edits them through the
// remember/forget tools (see internal/tool/memory.go). A per-file token budget
// keeps the injected text from bloating the prompt.
//
// L2 (SQLite FTS5 session search) and L3 (vector RAG) build on the shared
// state.db and land in later batches; see PLAN §10.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/tokens"
)

const (
	// MemoryFile holds the agent's long-term notes; UserFile holds the user
	// profile. Both live under Core.Dir.
	MemoryFile = "MEMORY.md"
	UserFile   = "USER.md"

	defaultMemoryBudget = 800 // tokens, see PLAN §10.5
	defaultUserBudget   = 500
)

// Target names a core-memory file the agent can write to.
type Target string

const (
	TargetMemory Target = "memory" // MEMORY.md — agent notes
	TargetUser   Target = "user"   // USER.md — user profile
)

// Core reads and writes the two core-memory files under a directory and renders
// them into a system instruction within a token budget. It is safe for the
// single-user CLI; concurrent writers are not coordinated (out of scope for L1).
type Core struct {
	dir          string
	memoryBudget int
	userBudget   int
}

// NewCore opens the core-memory directory, creating it if needed. An empty dir
// defaults to ~/.jelly-agent/memory; a leading ~ is expanded. Non-positive
// budgets fall back to the defaults.
func NewCore(dir string, memoryBudget, userBudget int) (*Core, error) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir: %w", err)
		}
		dir = filepath.Join(home, ".jelly-agent", "memory")
	} else {
		expanded, err := expandHome(dir)
		if err != nil {
			return nil, err
		}
		dir = expanded
	}
	if memoryBudget <= 0 {
		memoryBudget = defaultMemoryBudget
	}
	if userBudget <= 0 {
		userBudget = defaultUserBudget
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create memory dir: %w", err)
	}
	return &Core{dir: dir, memoryBudget: memoryBudget, userBudget: userBudget}, nil
}

// Dir reports the directory holding the core-memory files.
func (c *Core) Dir() string { return c.dir }

// Snapshot returns the current raw (untrimmed) contents of MEMORY.md and
// USER.md for display. Empty strings mean the file is empty or absent.
func (c *Core) Snapshot() (mem, user string) {
	return strings.TrimRight(c.read(MemoryFile), "\n"), strings.TrimRight(c.read(UserFile), "\n")
}

// Render prepends the budgeted core-memory sections to base, producing the
// system instruction for one turn. Empty files contribute nothing, so a fresh
// install behaves exactly like the static instruction.
func (c *Core) Render(base string) string {
	var b strings.Builder
	if usr := c.budgeted(UserFile, c.userBudget); usr != "" {
		b.WriteString("## 用户画像（USER.md）\n")
		b.WriteString(usr)
		b.WriteString("\n\n")
	}
	if mem := c.budgeted(MemoryFile, c.memoryBudget); mem != "" {
		b.WriteString("## 长期记忆（MEMORY.md）\n")
		b.WriteString(mem)
		b.WriteString("\n\n")
	}
	b.WriteString(base)
	return b.String()
}

// Remember appends fact as a bullet to the target file, creating it if needed.
// An exact duplicate (ignoring the bullet prefix) is not re-added.
func (c *Core) Remember(target Target, fact string) error {
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return fmt.Errorf("fact 不能为空")
	}
	name, err := fileFor(target)
	if err != nil {
		return err
	}
	for _, line := range c.entries(name) {
		if entryText(line) == fact {
			return nil // already remembered
		}
	}
	existing := strings.TrimRight(c.read(name), "\n")
	var b strings.Builder
	b.WriteString(existing)
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString("- ")
	b.WriteString(fact)
	b.WriteString("\n")
	if err := os.WriteFile(filepath.Join(c.dir, name), []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// Forget removes every entry in the target file containing match (case-
// insensitive substring), returning how many were removed.
func (c *Core) Forget(target Target, match string) (int, error) {
	match = strings.TrimSpace(match)
	if match == "" {
		return 0, fmt.Errorf("match 不能为空")
	}
	name, err := fileFor(target)
	if err != nil {
		return 0, err
	}
	content := strings.TrimRight(c.read(name), "\n")
	if content == "" {
		return 0, nil
	}
	needle := strings.ToLower(match)
	var kept []string
	removed := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(strings.ToLower(line), needle) {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if removed == 0 {
		return 0, nil
	}
	out := strings.Join(kept, "\n")
	if out != "" {
		out += "\n"
	}
	if err := os.WriteFile(filepath.Join(c.dir, name), []byte(out), 0o644); err != nil {
		return 0, fmt.Errorf("write %s: %w", name, err)
	}
	return removed, nil
}

// Set overwrites the target file with content (raw markdown), creating it if
// needed. It backs the web console's manual memory editor — the agent uses the
// finer-grained Remember/Forget. A trailing newline is normalized.
func (c *Core) Set(target Target, content string) error {
	name, err := fileFor(target)
	if err != nil {
		return err
	}
	content = strings.TrimRight(content, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(filepath.Join(c.dir, name), []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// budgeted reads name and trims it to fit budget tokens, keeping the most
// recent entries (files grow by appending). A marker notes any drop so the
// model knows memory was truncated rather than absent.
func (c *Core) budgeted(name string, budget int) string {
	content := strings.TrimRight(c.read(name), "\n")
	if content == "" {
		return ""
	}
	if estimateTokens(content) <= budget {
		return content
	}
	lines := strings.Split(content, "\n")
	var kept []string
	used := 0
	for i := len(lines) - 1; i >= 0; i-- {
		cost := estimateTokens(lines[i]) + 1 // +1 for the newline join
		if used+cost > budget && len(kept) > 0 {
			marker := fmt.Sprintf("（… 已省略较早的 %d 条记忆以控制上下文预算 …）", i+1)
			kept = append([]string{marker}, kept...)
			break
		}
		kept = append([]string{lines[i]}, kept...)
		used += cost
	}
	return strings.Join(kept, "\n")
}

// read returns the file's contents, or "" if it does not exist.
func (c *Core) read(name string) string {
	data, err := os.ReadFile(filepath.Join(c.dir, name))
	if err != nil {
		return ""
	}
	return string(data)
}

// entries returns the non-trimmed lines of a file (empty for a missing file).
func (c *Core) entries(name string) []string {
	content := strings.TrimRight(c.read(name), "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

// entryText strips a leading bullet ("- ") and surrounding space from a line.
func entryText(line string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
}

func fileFor(t Target) (string, error) {
	switch t {
	case TargetMemory, "":
		return MemoryFile, nil
	case TargetUser:
		return UserFile, nil
	default:
		return "", fmt.Errorf("未知 target %q（应为 memory 或 user）", t)
	}
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}

// estimateTokens approximates the token count of s. The implementation is
// shared with conversation-history compaction — see internal/tokens.
func estimateTokens(s string) int { return tokens.Estimate(s) }
