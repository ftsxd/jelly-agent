// Package skill implements Agent Skills (Claude/Anthropic style): each skill is
// a Markdown file with YAML frontmatter (name, description, enabled) plus a body
// of detailed instructions. The agent normally only sees the catalog (names +
// descriptions); it pulls a skill's full body on demand via the use_skill tool
// (progressive disclosure), keeping the base prompt small.
package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// nameRe restricts a skill name so it is a safe, stable file name and a clean
// identifier for the use_skill tool.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidName reports whether name is an allowed skill identifier/file name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Skill is one skill package: identity + on-demand instruction body.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Body        string `json:"body,omitempty"`
}

// frontmatter is the YAML header persisted at the top of each skill file.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Enabled     bool   `yaml:"enabled"`
}

// Store is a directory of skill Markdown files.
type Store struct {
	dir string
}

// NewStore opens the skills directory, creating it if needed. An empty dir
// defaults to ~/.jelly-agent/skills; a leading ~ is expanded.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir: %w", err)
		}
		dir = filepath.Join(home, ".jelly-agent", "skills")
	} else if strings.HasPrefix(dir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir: %w", err)
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create skills dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir reports the directory holding the skill files.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(name string) string { return filepath.Join(s.dir, name+".md") }

// List returns all skills (with bodies), sorted by name.
func (s *Store) List() ([]Skill, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		sk := parse(raw)
		if sk.Name == "" {
			sk.Name = strings.TrimSuffix(e.Name(), ".md")
		}
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get loads one skill by name.
func (s *Store) Get(name string) (Skill, bool, error) {
	if !ValidName(name) {
		return Skill{}, false, nil
	}
	raw, err := os.ReadFile(s.path(name))
	if err != nil {
		if os.IsNotExist(err) {
			return Skill{}, false, nil
		}
		return Skill{}, false, err
	}
	sk := parse(raw)
	if sk.Name == "" {
		sk.Name = name
	}
	return sk, true, nil
}

// Save writes a skill to <name>.md (0600 like the rest of the user data).
func (s *Store) Save(sk Skill) error {
	if !ValidName(sk.Name) {
		return fmt.Errorf("技能名仅允许字母、数字、下划线、连字符")
	}
	fm, err := yaml.Marshal(frontmatter{Name: sk.Name, Description: sk.Description, Enabled: sk.Enabled})
	if err != nil {
		return err
	}
	body := strings.TrimRight(sk.Body, "\n")
	content := "---\n" + string(fm) + "---\n\n" + body + "\n"
	if err := os.WriteFile(s.path(sk.Name), []byte(content), 0o600); err != nil {
		return fmt.Errorf("write skill %s: %w", sk.Name, err)
	}
	return nil
}

// Delete removes a skill file. Deleting a missing skill is not an error.
func (s *Store) Delete(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("技能名非法")
	}
	if err := os.Remove(s.path(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Catalog renders the enabled skills into an instruction block listing each
// skill's name and description, with a hint to call use_skill for the full
// steps. Returns "" when no skill is enabled (so nothing is injected).
func (s *Store) Catalog() (string, error) {
	all, err := s.List()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, sk := range all {
		if !sk.Enabled {
			continue
		}
		fmt.Fprintf(&b, "- %s：%s\n", sk.Name, sk.Description)
	}
	if b.Len() == 0 {
		return "", nil
	}
	return "## 可用技能\n" +
		"完成下列专项任务时，先调用 use_skill 工具（参数为技能名）获取该技能的完整步骤，再据此执行：\n" +
		b.String(), nil
}

// parse splits a skill file into its frontmatter and body. A file without valid
// frontmatter is treated as all-body (name/description empty).
func parse(raw []byte) Skill {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return Skill{Body: strings.TrimSpace(text)}
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return Skill{Body: strings.TrimSpace(text)}
	}
	var fm frontmatter
	_ = yaml.Unmarshal([]byte(rest[:end]), &fm)
	body := rest[end+len("\n---"):]
	body = strings.TrimPrefix(body, "\n")
	return Skill{Name: fm.Name, Description: fm.Description, Enabled: fm.Enabled, Body: strings.TrimSpace(body)}
}
