package tool

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// The file tools let an agent read a synced codebase. Every path they accept is
// resolved through Roots, so what the model can reach is exactly what the
// operator listed and nothing else — see fileroots.go for why containment is
// checked after symlink resolution.
//
// All three are read-only by design. Writing is what the sandbox exists for:
// a script under a policy, not a tool the model can call on impulse.

const (
	defaultReadLines = 400
	maxReadLines     = 2000
	// maxReadBytes caps one read regardless of the line count, because "400
	// lines" is no bound at all on a minified bundle that holds a megabyte on
	// one line.
	maxReadBytes = 256 << 10

	maxListEntries = 500

	defaultGrepMatches = 100
	maxGrepMatches     = 500
	maxGrepFiles       = 20000
	maxGrepFileBytes   = 4 << 20
	// grepLineCap trims a single matched line: a match inside a minified file
	// should cost one line of context, not the whole line.
	grepLineCap = 400

	// binarySniff is how much of a file is examined for NUL bytes before
	// deciding it is not text worth showing a model.
	binarySniff = 8 << 10
)

// skipDirs are directory names never worth walking for code analysis: they are
// large, machine-generated, and drown the signal in vendored copies.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true,
	"__pycache__": true, "dist": true, "build": true, "target": true,
	".idea": true, ".vscode": true, ".next": true, ".gradle": true,
}

// SkipDirNames lists the directory names grep_files never walks, so the console
// can show an operator why a vendored copy did not turn up in the results.
func SkipDirNames() []string {
	out := make([]string, 0, len(skipDirs))
	for d := range skipDirs {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// FileTools builds the read-only codebase tools for the given roots. It returns
// nothing when no root is configured: a tool that can only answer "no root" is
// worse than an absent one, because the model keeps trying it.
func FileTools(roots Roots) ([]adktool.Tool, error) {
	if roots.Empty() {
		return nil, nil
	}
	rf, err := newReadFileTool(roots)
	if err != nil {
		return nil, err
	}
	ld, err := newListDirTool(roots)
	if err != nil {
		return nil, err
	}
	gf, err := newGrepFilesTool(roots)
	if err != nil {
		return nil, err
	}
	return []adktool.Tool{rf, ld, gf}, nil
}

type readFileArgs struct {
	Path   string `json:"path" jsonschema:"要读取的文件路径，相对已配置的代码目录，例如 project-a/internal/x.go"`
	Offset int    `json:"offset,omitempty" jsonschema:"起始行号（从 1 开始），默认 1"`
	Limit  int    `json:"limit,omitempty" jsonschema:"最多返回多少行，默认 400，上限 2000"`
}

type readFileResult struct {
	Path       string `json:"path"`
	Content    string `json:"content"`             // numbered lines, so the model can cite them
	FromLine   int    `json:"from_line"`           // 1-based, inclusive
	ToLine     int    `json:"to_line"`             // 1-based, inclusive
	TotalLines int    `json:"total_lines"`         // of the whole file
	Truncated  bool   `json:"truncated,omitempty"` // more lines follow
	Note       string `json:"note,omitempty"`      // why the content is not the whole file
}

func newReadFileTool(roots Roots) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name:        "read_file",
			Description: "读取已同步代码库中某个文件的内容，按行返回并带行号。大文件用 offset/limit 分段读。只能访问已配置的代码目录。",
		},
		func(_ adktool.Context, args readFileArgs) (readFileResult, error) {
			return readFile(roots, args)
		},
	)
}

// readFile is the tool's body as a plain function, so the behaviour that
// matters — the caps, the offsets, the binary guard — can be tested without
// fabricating an ADK tool context.
func readFile(roots Roots, args readFileArgs) (readFileResult, error) {
	abs, err := roots.Resolve(args.Path)
	if err != nil {
		return readFileResult{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return readFileResult{}, fmt.Errorf("读取失败: %w", err)
	}
	if info.IsDir() {
		return readFileResult{}, fmt.Errorf("%s 是目录，请用 list_dir", roots.Display(abs))
	}

	f, err := os.Open(abs)
	if err != nil {
		return readFileResult{}, fmt.Errorf("读取失败: %w", err)
	}
	defer f.Close()

	if isBinary(f) {
		return readFileResult{Path: roots.Display(abs), Note: "二进制文件，未返回内容"}, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return readFileResult{}, err
	}

	from := args.Offset
	if from <= 0 {
		from = 1
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultReadLines
	}
	if limit > maxReadLines {
		limit = maxReadLines
	}

	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxReadBytes)
	line, shown, bytesOut := 0, 0, 0
	truncated, capped := false, false
	for sc.Scan() {
		line++
		if line < from {
			continue
		}
		if shown >= limit {
			truncated = true
			break
		}
		text := sc.Text()
		if bytesOut+len(text) > maxReadBytes {
			capped = true
			truncated = true
			break
		}
		b.WriteString(strconv.Itoa(line))
		b.WriteString("\t")
		b.WriteString(text)
		b.WriteString("\n")
		bytesOut += len(text)
		shown++
	}
	// Count the rest so the model knows whether to ask for more.
	for sc.Scan() {
		line++
	}
	if err := sc.Err(); err != nil {
		return readFileResult{}, fmt.Errorf("读取失败: %w", err)
	}

	res := readFileResult{
		Path: roots.Display(abs), Content: b.String(),
		FromLine: from, ToLine: from + shown - 1, TotalLines: line,
		Truncated: truncated,
	}
	if shown == 0 {
		res.FromLine, res.ToLine = 0, 0
		if line > 0 {
			res.Note = "offset 超出文件末尾（共 " + strconv.Itoa(line) + " 行）"
		}
	}
	if capped {
		res.Note = "单次读取已达 " + strconv.Itoa(maxReadBytes>>10) + " KB 上限"
	}
	return res, nil
}

type listDirArgs struct {
	Path string `json:"path,omitempty" jsonschema:"要列出的目录，相对已配置的代码目录；留空列出代码目录根"`
}

type dirEntry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
	// Label, Note and Tags are the operator's annotation for this entry, filled
	// in by the project tools. Browsing 130 bare service directory names tells a
	// model nothing; "order 订单服务 [核心链路]" tells it where to go next
	// without reading a line of code.
	Label string   `json:"label,omitempty"`
	Note  string   `json:"note,omitempty"`
	Tags  []string `json:"tags,omitempty"`
}

type listDirResult struct {
	Path      string     `json:"path"`
	Entries   []dirEntry `json:"entries"`
	Truncated bool       `json:"truncated,omitempty"`
}

func newListDirTool(roots Roots) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name:        "list_dir",
			Description: "列出已同步代码库中某个目录的内容（目录在前，按名字排序）。留空 path 则列出代码目录根。只能访问已配置的代码目录。",
		},
		func(_ adktool.Context, args listDirArgs) (listDirResult, error) {
			return listDir(roots, args)
		},
	)
}

// listDir is the tool's body as a plain function — see readFile.
func listDir(roots Roots, args listDirArgs) (listDirResult, error) {
	// An empty path lists the roots themselves, so the model has a way
	// to discover which projects exist before it knows any names.
	if strings.TrimSpace(args.Path) == "" && len(roots.Dirs()) > 1 {
		var entries []dirEntry
		for _, d := range roots.Dirs() {
			entries = append(entries, dirEntry{Name: filepath.Base(d), Dir: true})
		}
		return listDirResult{Path: ".", Entries: entries}, nil
	}
	path := args.Path
	if strings.TrimSpace(path) == "" {
		path = roots.Dirs()[0]
	}
	abs, err := roots.Resolve(path)
	if err != nil {
		return listDirResult{}, err
	}

	// A directory that is only an ancestor of the project's scope is a
	// navigation node: it shows the way down and nothing else. Listing the
	// other 125 services under services/ would hand the model paths it is not
	// allowed to read and invite it to try them.
	if !roots.AllowsDir(abs) && roots.IsNavigation(abs) {
		res := listDirResult{Path: roots.Display(abs)}
		for _, name := range roots.NavigationChildren(abs) {
			res.Entries = append(res.Entries, dirEntry{Name: name, Dir: true})
		}
		sort.SliceStable(res.Entries, func(i, j int) bool { return res.Entries[i].Name < res.Entries[j].Name })
		return res, nil
	}

	items, err := os.ReadDir(abs)
	if err != nil {
		return listDirResult{}, fmt.Errorf("列目录失败: %w", err)
	}

	res := listDirResult{Path: roots.Display(abs)}
	for _, it := range items {
		if len(res.Entries) >= maxListEntries {
			res.Truncated = true
			break
		}
		e := dirEntry{Name: it.Name(), Dir: it.IsDir()}
		if !it.IsDir() {
			if info, err := it.Info(); err == nil {
				e.Bytes = info.Size()
			}
		}
		res.Entries = append(res.Entries, e)
	}
	sort.SliceStable(res.Entries, func(i, j int) bool {
		if res.Entries[i].Dir != res.Entries[j].Dir {
			return res.Entries[i].Dir
		}
		return res.Entries[i].Name < res.Entries[j].Name
	})
	return res, nil
}

type grepFilesArgs struct {
	Pattern    string `json:"pattern" jsonschema:"要搜索的正则表达式（Go 语法）"`
	Path       string `json:"path,omitempty" jsonschema:"限定搜索的子目录，留空则搜索全部代码目录"`
	Glob       string `json:"glob,omitempty" jsonschema:"按文件名过滤，例如 *.go"`
	MaxMatches int    `json:"max_matches,omitempty" jsonschema:"最多返回多少条匹配，默认 100，上限 500"`
}

type grepMatch struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type grepFilesResult struct {
	Matches     []grepMatch `json:"matches"`
	FilesSearch int         `json:"files_searched"`
	Truncated   bool        `json:"truncated,omitempty"`
	Note        string      `json:"note,omitempty"`
}

func newGrepFilesTool(roots Roots) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name:        "grep_files",
			Description: "在已同步代码库里按正则搜索，返回 文件:行号:内容。用它定位代码，再用 read_file 读上下文。自动跳过 .git/node_modules/vendor 等目录和二进制文件。",
		},
		func(_ adktool.Context, args grepFilesArgs) (grepFilesResult, error) {
			return grepFiles(roots, args)
		},
	)
}

// grepFiles is the tool's body as a plain function — see readFile.
func grepFiles(roots Roots, args grepFilesArgs) (grepFilesResult, error) {
	if strings.TrimSpace(args.Pattern) == "" {
		return grepFilesResult{}, fmt.Errorf("pattern 不能为空")
	}
	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return grepFilesResult{}, fmt.Errorf("正则无效: %w", err)
	}
	if args.Glob != "" {
		if _, err := filepath.Match(args.Glob, "probe"); err != nil {
			return grepFilesResult{}, fmt.Errorf("glob 无效: %w", err)
		}
	}
	targets, err := roots.ResolveOrRoots(args.Path)
	if err != nil {
		return grepFilesResult{}, err
	}
	max := args.MaxMatches
	if max <= 0 {
		max = defaultGrepMatches
	}
	if max > maxGrepMatches {
		max = maxGrepMatches
	}

	res := grepFilesResult{Matches: []grepMatch{}}
	for _, target := range targets {
		stop := walkAndGrep(target, roots, re, args.Glob, max, &res)
		if stop {
			break
		}
	}
	if res.FilesSearch >= maxGrepFiles {
		res.Note = "扫描文件数已达上限 " + strconv.Itoa(maxGrepFiles) + "，结果可能不完整"
	}
	return res, nil
}

// walkAndGrep searches one subtree, appending to res. It reports whether the
// caller should stop entirely (match or file budget exhausted).
func walkAndGrep(target string, roots Roots, re *regexp.Regexp, glob string, max int, res *grepFilesResult) bool {
	stop := false
	_ = filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, don't abort the search
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			// Stay inside the project's configured directories even when the
			// walk started from an ancestor the model was allowed to name.
			if !roots.AllowsDir(path) && !roots.IsNavigation(path) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // don't follow symlinks out of the tree
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, d.Name()); !ok {
				return nil
			}
		}
		if res.FilesSearch >= maxGrepFiles {
			stop = true
			return fs.SkipAll
		}
		if info, err := d.Info(); err == nil && info.Size() > maxGrepFileBytes {
			return nil
		}
		res.FilesSearch++

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		if isBinary(f) {
			return nil
		}
		if _, err := f.Seek(0, 0); err != nil {
			return nil
		}

		display := roots.Display(path)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), maxGrepFileBytes)
		line := 0
		for sc.Scan() {
			line++
			text := sc.Text()
			if !re.MatchString(text) {
				continue
			}
			if len(res.Matches) >= max {
				res.Truncated = true
				stop = true
				return fs.SkipAll
			}
			if len(text) > grepLineCap {
				text = text[:grepLineCap] + "…"
			}
			res.Matches = append(res.Matches, grepMatch{File: display, Line: line, Text: text})
		}
		return nil
	})
	return stop
}

// isBinary reports whether the file looks like something other than text. A NUL
// byte in the first few KB is the same heuristic grep uses, and it is what keeps
// a compiled artifact out of the model's context.
func isBinary(f *os.File) bool {
	buf := make([]byte, binarySniff)
	n, _ := f.Read(buf)
	return bytes.IndexByte(buf[:n], 0) >= 0
}
