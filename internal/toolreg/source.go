package toolreg

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// Source supplies tool metadata and reports when it changes.
//
// Watch is part of the interface rather than a later addition because the
// interesting sources push: a config service notifies, and tuning selection
// quality means editing this metadata repeatedly. An interface with only Load
// would force every caller to change the day a pushing source arrives, which
// is the opposite of what an interface is for.
//
// A file-backed source implements Watch by polling; that is an implementation
// detail callers never see.
type Source interface {
	// Name identifies the source in logs and health output.
	Name() string
	// Load reads the current metadata.
	Load(ctx context.Context) ([]ops.ToolMetadata, error)
	// Watch delivers a fresh set whenever the underlying data changes. The
	// channel closes when ctx is done. A source with no change notification
	// may return a channel that never yields.
	Watch(ctx context.Context) <-chan []ops.ToolMetadata
}

// filePollInterval is how often FileSource re-stats its files. Metadata edits
// are a human activity; a second of latency is invisible and the cost is a
// stat call.
const filePollInterval = 2 * time.Second

// FileSource reads metadata from YAML files in a directory.
//
// One file per backend keeps diffs readable and review meaningful — the point
// of holding this in a file at all is that a change to how a tool is described
// shows up in review like any other change.
type FileSource struct {
	dir string

	mu   sync.Mutex
	sigs map[string]string // path -> mtime+size, to detect edits
}

// NewFileSource reads *.yaml and *.yml from dir. A missing directory is not an
// error: metadata is optional, and a deployment with none should still start.
func NewFileSource(dir string) *FileSource {
	return &FileSource{dir: dir, sigs: map[string]string{}}
}

func (f *FileSource) Name() string { return "file:" + f.dir }

// ConsoleFile is the file the console used to own.
//
// The console's layer is tool_decls now — see DBSource for why. The name
// survives for one purpose: finding an old file on an upgrading deployment
// and importing it (ImportConsoleFile). This package no longer treats it as
// an overlay, because there is only one overlay and it is the database; a
// file that reappeared here would otherwise be a second one, silently
// competing with what the console shows.
const ConsoleFile = "console.yaml"

// metadataFile is the on-disk shape. The wrapper key exists so a file can
// later carry defaults or a version without breaking the parse.
type metadataFile struct {
	Tools []ops.ToolMetadata `yaml:"tools"`
}

// Load parses every file in the directory.
//
// A malformed file fails the whole load rather than being skipped. Skipping
// would leave the registry quietly missing whatever that file defined, and the
// symptom — a tool that is configured but absent — is far harder to trace back
// than a parse error naming the file.
//
// Every file here is a base declaration. The overlay — the console's layer —
// lives in the database now; see DBSource.
func (f *FileSource) Load(ctx context.Context) ([]ops.ToolMetadata, error) {
	paths, err := f.paths()
	if err != nil {
		return nil, err
	}
	var base []ops.ToolMetadata
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("toolreg: read %s: %w", p, err)
		}
		var mf metadataFile
		if err := yaml.Unmarshal(raw, &mf); err != nil {
			return nil, fmt.Errorf("toolreg: parse %s: %w", p, err)
		}
		base = append(base, mf.Tools...)
	}
	return base, nil
}

// applyOverlay patches declarations onto the entries they name.
//
// The console's file carries a declaration patch: model-facing retrieval text,
// suites, what a tool produces, and whether calling it changes anything.
//
// So it cannot be an ordinary entry. Registration is first-wins per key, so
// making the console's file load first — which is what it took for a
// declaration to beat a hand-written one — meant its four fields stood in for
// the whole entry, and everything else that entry carried was dropped as a
// duplicate: the description the model reads, the timeout, the result ceiling,
// the aliases, the injected parameters. Declaring that a tool returns metrics
// would quietly remove its own description.
//
// A declaration for a tool no file mentions stands on its own, which is the
// common case: MCP tools have no file entry at all unless somebody wrote one.
func applyOverlay(base, overlay []ops.ToolMetadata) []ops.ToolMetadata {
	if len(overlay) == 0 {
		return base
	}
	at := make(map[string]int, len(base))
	for i, m := range base {
		at[m.Key()] = i
	}
	out := append([]ops.ToolMetadata(nil), base...)
	for _, o := range overlay {
		i, found := at[o.Key()]
		if !found {
			at[o.Key()] = len(out)
			out = append(out, o)
			continue
		}
		// Only what the overlay actually declared. An unset field means "the
		// console has nothing to say about this", not "clear it" — clearing a
		// declaration removes the whole entry from the console's file, which
		// is what makes the underlying one apply again.
		if o.Produces != "" {
			out[i].Produces = o.Produces
		}
		if o.SideEffect != "" {
			out[i].SideEffect = o.SideEffect
		}
		if o.Description != "" {
			out[i].Description = o.Description
		}
		if len(o.UseCases) > 0 {
			out[i].UseCases = append([]string(nil), o.UseCases...)
		}
		if len(o.Examples) > 0 {
			out[i].Examples = append([]string(nil), o.Examples...)
		}
		if len(o.AntiExamples) > 0 {
			out[i].AntiExamples = append([]string(nil), o.AntiExamples...)
		}
		if len(o.Suites) > 0 {
			out[i].Suites = append([]string(nil), o.Suites...)
		}
	}
	return out
}

// Watch polls for edits and re-loads on change.
func (f *FileSource) Watch(ctx context.Context) <-chan []ops.ToolMetadata {
	ch := make(chan []ops.ToolMetadata, 1)
	go func() {
		defer close(ch)
		t := time.NewTicker(filePollInterval)
		defer t.Stop()
		f.changed() // establish a baseline so the first tick is not a false positive
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !f.changed() {
					continue
				}
				metas, err := f.Load(ctx)
				if err != nil {
					// A half-saved file parses badly for a moment; the next
					// tick picks up the finished version. Reporting is the
					// caller's job — it has the logger.
					continue
				}
				select {
				case ch <- metas:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch
}

func (f *FileSource) paths() ([]string, error) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no metadata configured
		}
		return nil, fmt.Errorf("toolreg: read dir %s: %w", f.dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".yaml", ".yml":
			out = append(out, filepath.Join(f.dir, e.Name()))
		}
	}
	sort.Strings(out) // deterministic load order, so conflicts are reproducible
	return out, nil
}

// changed reports whether any file appeared, vanished or was modified.
func (f *FileSource) changed() bool {
	paths, err := f.paths()
	if err != nil {
		return false
	}
	next := make(map[string]string, len(paths))
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		next[p] = fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size())
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	same := len(next) == len(f.sigs)
	if same {
		for p, sig := range next {
			if f.sigs[p] != sig {
				same = false
				break
			}
		}
	}
	f.sigs = next
	return !same
}

// StaticSource serves a fixed set. It is what built-in tool metadata uses, and
// what a test injects.
type StaticSource struct {
	Label string
	Metas []ops.ToolMetadata
}

func (s StaticSource) Name() string { return "static:" + s.Label }

func (s StaticSource) Load(context.Context) ([]ops.ToolMetadata, error) { return s.Metas, nil }

func (s StaticSource) Watch(ctx context.Context) <-chan []ops.ToolMetadata {
	ch := make(chan []ops.ToolMetadata)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}

// StaticOverlay is a fixed set applied as a patch over the others, which is
// what a set already read out of the database is.
//
// The overlay marker is what decides layering (see Overlay), so a caller
// holding declarations it has already loaded can install them without
// loading them again — and "already loaded" is exactly what makes the
// install reliable enough to confirm a version against.
type StaticOverlay struct {
	StaticSource
}

// IsOverlay marks this as a patch over the file sources.
func (StaticOverlay) IsOverlay() {}

// Merge loads every source in order and concatenates the results.
//
// Order is significance: a later source's entry conflicts with an earlier
// one's and loses, so the built-in static source goes first and remote
// overlays follow. Build reports every conflict, so a losing entry is never
// silent.
func Merge(ctx context.Context, sources ...Source) ([]ops.ToolMetadata, error) {
	var base, overlay []ops.ToolMetadata
	for _, s := range sources {
		metas, err := s.Load(ctx)
		if err != nil {
			return nil, fmt.Errorf("toolreg: source %s: %w", s.Name(), err)
		}
		if _, ok := s.(Overlay); ok {
			overlay = append(overlay, metas...)
			continue
		}
		base = append(base, metas...)
	}
	// Overlays last, and patched in rather than appended, so an operator who
	// set one field in the console does not silently discard what a
	// hand-written file said about the others. See applyOverlay.
	return applyOverlay(base, overlay), nil
}

// Overlay marks a source whose entries patch the others rather than compete
// with them.
//
// The distinction is not about precedence — Build already resolves that — but
// about granularity. A base source declares a tool; an overlay declares a
// field of one, and the fields it says nothing about must keep the value the
// base gave them. Two sources both declaring the same tool would otherwise be
// a conflict, and resolving it either way loses half the answer.
type Overlay interface {
	Source
	// IsOverlay is a marker. It exists so a source opts in explicitly rather
	// than being classified by its name or its position in a list.
	IsOverlay()
}
