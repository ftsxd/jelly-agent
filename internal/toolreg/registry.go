// Package toolreg holds the tool registry: our own metadata for every tool,
// keyed by where the tool actually lives, and resolvable by every name the
// model might use.
//
// The registry is the answer to two problems that arrive together once MCP
// servers are involved.
//
// The first is naming. ADK's MCP adapter uses one field as both the name the
// model sees and the routing key back to the server (tool/mcptoolset/tool.go),
// so two Kubernetes servers each exposing get_pods are indistinguishable; the
// failure surfaces as "duplicate tool" while assembling a request, several
// turns after the server was added, and without naming either culprit. This
// package keeps our name and the remote name apart, resolves aliases
// many-to-one, and rejects a conflict at registration with an error that says
// which two entries collided.
//
// The second is context size. The registry may hold hundreds of tools; only
// the handful handed to the model costs tokens. Keeping the catalogue here —
// in memory, invisible to the model — is what decouples registry size from
// per-turn prompt cost. Nothing in this package sends anything to a model.
package toolreg

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// Registry is an immutable snapshot of tool metadata.
//
// Immutable on purpose: metadata is hot-reloadable, and a diagnosis that
// started under one snapshot must finish under it. Mutating a shared map
// mid-run would change a tool's definition between two calls of the same
// investigation, producing behaviour no one can reproduce.
type Registry struct {
	byKey  map[string]ops.ToolMetadata // "server/remote" -> metadata
	byName map[string]string           // every resolvable name -> key
	order  []string                    // keys, stable for deterministic listing
}

// Store holds the current snapshot and swaps it atomically.
type Store struct{ cur atomic.Pointer[Registry] }

// NewStore returns a store holding an empty registry, so callers never have to
// handle a nil snapshot.
func NewStore() *Store {
	s := &Store{}
	s.cur.Store(&Registry{byKey: map[string]ops.ToolMetadata{}, byName: map[string]string{}})
	return s
}

// Load returns the current snapshot. Callers should read it once per run and
// keep using that value.
func (s *Store) Load() *Registry { return s.cur.Load() }

// Swap installs a new snapshot. Runs already in flight keep the old one.
func (s *Store) Swap(r *Registry) { s.cur.Store(r) }

// ConflictKind says what went wrong, because the three cases need different
// fixes and one message for all of them helps with none.
type ConflictKind string

const (
	// ConflictName: two entries want the same model-visible name. Rename one.
	ConflictName ConflictKind = "name"
	// ConflictDuplicateKey: the same server exposed the same remote name
	// twice — a malformed config or a double registration, not a naming
	// decision anyone made.
	ConflictDuplicateKey ConflictKind = "duplicate_key"
	// ConflictNoName: neither Name nor RemoteName was set.
	ConflictNoName ConflictKind = "no_name"
)

// Conflict describes an entry that could not be registered.
//
// For a name clash it distinguishes colliding with another tool's own name
// from colliding with its alias, because the fix differs: the first needs one
// of the two renamed, the second needs an alias dropped. And it always names
// both sides — ADK's own "duplicate tool: %q" names neither, which is exactly
// what makes that error so hard to act on.
type Conflict struct {
	Kind ConflictKind `json:"kind"`
	Name string       `json:"name,omitempty"` // the contested name
	Key  string       `json:"key"`            // the entry that could not be registered

	ExistingKey     string `json:"existing_key,omitempty"` // who already holds the name
	AsAlias         bool   `json:"as_alias,omitempty"`     // Name is an alias of Key
	ExistingIsAlias bool   `json:"existing_is_alias,omitempty"`
}

func (c Conflict) Error() string {
	switch c.Kind {
	case ConflictDuplicateKey:
		return fmt.Sprintf("工具 %s 重复注册（同一 server 的同名远端工具出现了两次）", c.Key)
	case ConflictNoName:
		return fmt.Sprintf("工具条目缺少名称（server=%q）", c.ExistingKey)
	default:
		mine, theirs := "名称", "名称"
		if c.AsAlias {
			mine = "别名"
		}
		if c.ExistingIsAlias {
			theirs = "别名"
		}
		return fmt.Sprintf("工具 %s 的%s %q 与 %s 的%s冲突", c.Key, mine, c.Name, c.ExistingKey, theirs)
	}
}

// Build assembles a registry, rejecting the whole batch if any name collides.
//
// All-or-nothing is deliberate. A registry that silently dropped the losing
// entry would leave an operator staring at a tool that is configured, enabled,
// and absent — with nothing to explain why.
func Build(metas []ops.ToolMetadata) (*Registry, []Conflict) {
	r := &Registry{
		byKey:  make(map[string]ops.ToolMetadata, len(metas)),
		byName: make(map[string]string, len(metas)*2),
	}
	// Track whether a registered name was the entry's own or an alias, so a
	// later conflict can say which.
	aliasName := map[string]bool{}
	var conflicts []Conflict

	for _, m := range metas {
		if m.Name == "" && m.RemoteName == "" {
			conflicts = append(conflicts, Conflict{Kind: ConflictNoName, ExistingKey: m.Server})
			continue
		}
		if m.Name == "" {
			m.Name = m.RemoteName
		}
		key := m.Key()
		if _, dup := r.byKey[key]; dup {
			conflicts = append(conflicts, Conflict{Kind: ConflictDuplicateKey, Key: key})
			continue
		}

		// Claim every name this entry answers to before committing it, so a
		// half-registered entry cannot exist.
		claimed := make([]string, 0, 1+len(m.Aliases))
		clash := false
		for i, name := range m.Names() {
			if existing, taken := r.byName[name]; taken {
				conflicts = append(conflicts, Conflict{
					Kind: ConflictName,
					Name: name, Key: key, ExistingKey: existing,
					AsAlias:         i > 0,
					ExistingIsAlias: aliasName[name],
				})
				clash = true
				break
			}
			claimed = append(claimed, name)
		}
		if clash {
			continue
		}

		for i, name := range claimed {
			r.byName[name] = key
			aliasName[name] = i > 0
		}
		r.byKey[key] = m
		r.order = append(r.order, key)
	}

	sort.Strings(r.order)
	return r, conflicts
}

// Lookup resolves any name — canonical or alias — to its metadata.
func (r *Registry) Lookup(name string) (ops.ToolMetadata, bool) {
	if r == nil {
		return ops.ToolMetadata{}, false
	}
	key, ok := r.byName[name]
	if !ok {
		return ops.ToolMetadata{}, false
	}
	m, ok := r.byKey[key]
	return m, ok
}

// ByKey resolves a "server/remote" key.
func (r *Registry) ByKey(key string) (ops.ToolMetadata, bool) {
	if r == nil {
		return ops.ToolMetadata{}, false
	}
	m, ok := r.byKey[key]
	return m, ok
}

// All lists every entry in a stable order.
func (r *Registry) All() []ops.ToolMetadata {
	if r == nil {
		return nil
	}
	out := make([]ops.ToolMetadata, 0, len(r.order))
	for _, k := range r.order {
		out = append(out, r.byKey[k])
	}
	return out
}

// Len is the number of registered tools — the catalogue size, which is not
// what the model pays for.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.byKey)
}

// ForServer lists the entries belonging to one MCP server.
func (r *Registry) ForServer(server string) []ops.ToolMetadata {
	if r == nil {
		return nil
	}
	var out []ops.ToolMetadata
	for _, k := range r.order {
		if m := r.byKey[k]; m.Server == server {
			out = append(out, m)
		}
	}
	return out
}

// Available filters the catalogue down to what could run for this incident.
//
// This is the cheap, lossless cut that has to happen before any scoring: a
// tool whose backend has no handle in this context cannot be called at all, so
// dropping it removes a candidate without removing an option. A Kubernetes
// memory alert has no MySQL instance, and the MySQL tools were never in play.
//
// A tool with no declared backend (a built-in like web_search) is always
// available: it does not address infrastructure.
func (r *Registry) Available(c *ops.IncidentContext) []ops.ToolMetadata {
	if r == nil {
		return nil
	}
	backends := map[string]bool{}
	for _, b := range c.Backends() {
		backends[b] = true
	}
	var out []ops.ToolMetadata
	for _, k := range r.order {
		m := r.byKey[k]
		if m.Backend == "" || backends[m.Backend] {
			out = append(out, m)
		}
	}
	return out
}

// Names lists every resolvable name, for a health endpoint or a conflict
// report.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// CheckAgainst reports the conflicts that adding metas to this registry would
// cause, without modifying anything.
//
// This is what an API handler calls before saving a server: a clash found here
// is reported at the moment of the change, naming both sides, instead of
// surfacing as an opaque failure during someone's next conversation.
func (r *Registry) CheckAgainst(metas []ops.ToolMetadata) []Conflict {
	existing := r.All()
	_, conflicts := Build(append(existing, metas...))
	// Filter to conflicts the new batch caused: a registry that was already
	// inconsistent is a separate problem, and reporting it here would blame
	// the wrong change.
	incoming := map[string]bool{}
	for _, m := range metas {
		if m.Name == "" {
			m.Name = m.RemoteName
		}
		incoming[m.Key()] = true
	}
	var out []Conflict
	for _, c := range conflicts {
		if incoming[c.Key] {
			out = append(out, c)
		}
	}
	return out
}

// FormatConflicts renders conflicts as one message, for an API error body.
func FormatConflicts(cs []Conflict) string {
	if len(cs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.Error())
	}
	return strings.Join(parts, "；")
}
