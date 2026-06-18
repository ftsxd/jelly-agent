// Package skill implements Agent Skills (Claude/Anthropic style): each skill is
// a Markdown file with YAML frontmatter (name, description, enabled) plus a body
// of detailed instructions. The agent normally only sees the catalog (names +
// descriptions); it pulls a skill's full body on demand via the use_skill tool
// (progressive disclosure), keeping the base prompt small.
package skill

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path"
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

// A skill lives in one of two layouts: a flat file <dir>/<name>.md (created via
// the web form) or a directory <dir>/<name>/SKILL.md (imported from a zip, which
// may also carry bundled resource files). resolve returns the backing file,
// preferring the directory form when present.
func (s *Store) flatPath(name string) string { return filepath.Join(s.dir, name+".md") }
func (s *Store) dirSkillPath(name string) string {
	return filepath.Join(s.dir, name, "SKILL.md")
}
func (s *Store) resolve(name string) string {
	if _, err := os.Stat(s.dirSkillPath(name)); err == nil {
		return s.dirSkillPath(name)
	}
	return s.flatPath(name)
}

// List returns all skills (with bodies), sorted by name. It reads both flat
// <name>.md files and <name>/SKILL.md directory skills.
func (s *Store) List() ([]Skill, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, e := range entries {
		var raw []byte
		var fallback string
		if e.IsDir() {
			b, err := os.ReadFile(filepath.Join(s.dir, e.Name(), "SKILL.md"))
			if err != nil {
				continue // not a skill directory
			}
			raw, fallback = b, e.Name()
		} else if strings.HasSuffix(e.Name(), ".md") {
			b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
			if err != nil {
				continue
			}
			raw, fallback = b, strings.TrimSuffix(e.Name(), ".md")
		} else {
			continue
		}
		sk := parse(raw)
		if sk.Name == "" {
			sk.Name = fallback
		}
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get loads one skill by name (directory form preferred).
func (s *Store) Get(name string) (Skill, bool, error) {
	if !ValidName(name) {
		return Skill{}, false, nil
	}
	raw, err := os.ReadFile(s.resolve(name))
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

// Save writes a skill (0600). It updates the directory form in place when the
// skill was imported as a directory, otherwise writes the flat <name>.md.
func (s *Store) Save(sk Skill) error {
	if !ValidName(sk.Name) {
		return fmt.Errorf("技能名仅允许字母、数字、下划线、连字符")
	}
	target := s.flatPath(sk.Name)
	if _, err := os.Stat(s.dirSkillPath(sk.Name)); err == nil {
		target = s.dirSkillPath(sk.Name) // keep bundled resources, edit SKILL.md
	}
	if err := os.WriteFile(target, []byte(render(sk)), 0o600); err != nil {
		return fmt.Errorf("write skill %s: %w", sk.Name, err)
	}
	return nil
}

// Delete removes a skill in either layout. Deleting a missing skill is not an
// error.
func (s *Store) Delete(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("技能名非法")
	}
	if err := os.Remove(s.flatPath(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.dir, name)); err != nil {
		return err
	}
	return nil
}

// Zip import limits — a skill is markdown + small reference files, not a payload.
const (
	maxZipFiles    = 100
	maxZipFileSize = 2 << 20  // 2 MiB per file
	maxZipTotal    = 10 << 20 // 10 MiB extracted
)

// ImportZip extracts a skill package from a zip. The zip must contain a SKILL.md
// (at the root or inside a single top-level folder); its frontmatter `name`
// becomes the skill identifier. All files under that folder are extracted to
// <dir>/<name>/ (preserving bundled resources), guarded against path traversal
// and size abuse. The imported skill is normalized to enabled=true.
func (s *Store) ImportZip(r io.ReaderAt, size int64) (Skill, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return Skill{}, fmt.Errorf("无效的 zip: %w", err)
	}

	// Locate the shallowest SKILL.md and the folder prefix to strip.
	var skillFile *zip.File
	for _, f := range zr.File {
		if strings.EqualFold(path.Base(f.Name), "SKILL.md") {
			if skillFile == nil || strings.Count(f.Name, "/") < strings.Count(skillFile.Name, "/") {
				skillFile = f
			}
		}
	}
	if skillFile == nil {
		return Skill{}, fmt.Errorf("zip 中未找到 SKILL.md")
	}
	prefix := path.Dir(skillFile.Name)
	if prefix == "." {
		prefix = ""
	} else {
		prefix += "/"
	}

	mdRaw, err := readZipEntry(skillFile)
	if err != nil {
		return Skill{}, err
	}
	sk := parse(mdRaw)
	if !ValidName(sk.Name) {
		return Skill{}, fmt.Errorf("SKILL.md 的 name 缺失或非法（仅允许字母、数字、下划线、连字符）")
	}
	sk.Enabled = true // a freshly uploaded skill is enabled by default

	target := filepath.Join(s.dir, sk.Name)
	if err := os.RemoveAll(target); err != nil {
		return Skill{}, err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return Skill{}, err
	}

	var total int64
	count := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel := f.Name
		if prefix != "" {
			if !strings.HasPrefix(rel, prefix) {
				continue // outside the skill folder
			}
			rel = strings.TrimPrefix(rel, prefix)
		}
		if rel == "" || strings.EqualFold(rel, "SKILL.md") {
			continue // SKILL.md is written normalized below
		}
		dest := filepath.Join(target, filepath.FromSlash(rel))
		if relPath, err := filepath.Rel(target, dest); err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator)) {
			continue // zip-slip guard: skip anything escaping target
		}
		if count++; count > maxZipFiles {
			return Skill{}, fmt.Errorf("zip 内文件过多（上限 %d）", maxZipFiles)
		}
		data, err := readZipEntry(f)
		if err != nil {
			return Skill{}, err
		}
		if total += int64(len(data)); total > maxZipTotal {
			return Skill{}, fmt.Errorf("zip 解压内容过大（上限 %d MiB）", maxZipTotal>>20)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return Skill{}, err
		}
		if err := os.WriteFile(dest, data, 0o600); err != nil {
			return Skill{}, err
		}
	}

	// Write the normalized SKILL.md (consistent frontmatter, enabled=true).
	if err := os.WriteFile(s.dirSkillPath(sk.Name), []byte(render(sk)), 0o600); err != nil {
		return Skill{}, err
	}
	return sk, nil
}

// readZipEntry reads one zip file, rejecting oversized entries.
func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxZipFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxZipFileSize {
		return nil, fmt.Errorf("zip 内文件过大（上限 %d MiB）: %s", maxZipFileSize>>20, f.Name)
	}
	return data, nil
}

// render serializes a skill to its file form (YAML frontmatter + body).
func render(sk Skill) string {
	fm, _ := yaml.Marshal(frontmatter{Name: sk.Name, Description: sk.Description, Enabled: sk.Enabled})
	return "---\n" + string(fm) + "---\n\n" + strings.TrimRight(sk.Body, "\n") + "\n"
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
