package server

// Declaring what a tool is, from the console.
//
// An MCP server reports a name and a description. It does not report what kind
// of evidence a tool produces or whether calling it changes anything, and both
// of those are things this system reasons with: selection scores by them, the
// gateway's ceiling policy reads them, and the task view groups work by them.
// Without a declaration every MCP tool is "执行工具" — honest, and useless.
//
// The registry has always been able to take these declarations from a file.
// What was missing was somewhere to put the file that anyone would find, and
// any way to write one without a text editor. That is all this is.

import (
	"context"
	"github.com/jelly-agent/jelly-agent/internal/storage"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/toolreg"
)

// toolDeclDTO is one tool's declaration as the console edits it.
//
// A deliberately scoped slice of ToolMetadata: the fields that affect model
// selection plus evidence and side-effect declarations. Offering the rest
// would invite editing fields whose consequences are not visible here.
type toolDeclDTO struct {
	Name         string   `json:"name"`
	Server       string   `json:"server,omitempty"`
	Description  string   `json:"description,omitempty"`
	UseCases     []string `json:"use_cases,omitempty"`
	Examples     []string `json:"examples,omitempty"`
	AntiExamples []string `json:"anti_examples,omitempty"`
	Suites       []string `json:"suites,omitempty"`
	// Declared is what the console's own file says, field by field — as
	// opposed to the fields above, which are what the registry resolved.
	//
	// The editor needs both and they are not the same thing. Filling its
	// boxes with the resolved value made every inherited field an explicit
	// override the moment somebody opened the editor and saved: an MCP
	// server's own description got copied into the override file, and a
	// field the operator never touched got written back from whatever the
	// page happened to be showing. Nil means the console declares nothing
	// for this tool, which is not the same as declaring it empty.
	Declared *toolDeclFields `json:"declared,omitempty"`
	// Produces and Effect are what the registry actually resolved, which is
	// not always what the console asked for — see Shadowed.
	Produces string `json:"produces,omitempty"`
	Effect   string `json:"side_effect,omitempty"`
	Source   string `json:"source,omitempty"` // "console" | "file" | "builtin"
	// Shadowed says the console declared this tool and something else won.
	//
	// The console's file is loaded first, so this should not happen — but
	// "should not" is what the old source field asserted by checking only
	// whether the console file contained the key, which made the page say
	// 已声明 while the gateway used another file's answer. Reported by
	// comparing the declaration against what the registry resolved, so the
	// page can only claim what is true.
	Shadowed bool `json:"shadowed,omitempty"`
}

// toolDeclFields is the console's own declaration for one tool: the selection
// fields it may override, and nothing resolved on its behalf.
type toolDeclFields struct {
	Description  string   `json:"description,omitempty"`
	UseCases     []string `json:"use_cases,omitempty"`
	Examples     []string `json:"examples,omitempty"`
	AntiExamples []string `json:"anti_examples,omitempty"`
	Suites       []string `json:"suites,omitempty"`
}

// handleToolMetadata lists what has been declared, and where each came from.
func (s *Server) handleToolMetadata(w http.ResponseWriter, r *http.Request) {
	db, ok := s.stateDB(w)
	if !ok {
		return
	}
	declared, err := toolreg.NewDBSource(db).Load(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	fromConsole := make(map[string]ops.ToolMetadata, len(declared))
	for _, m := range declared {
		fromConsole[declKey(m.Server, m.Name)] = m
	}

	out := []toolDeclDTO{}
	if reg := s.engine().ToolRegistry(); reg != nil {
		for _, name := range reg.Names() {
			m, ok := reg.Lookup(name)
			if !ok || m.Name != name {
				continue // aliases resolve to the same entry; list it once
			}
			decl, inConsole := fromConsole[declKey(m.Server, m.Name)]
			out = append(out, declRow(m, decl, inConsole, sourceOf(m, inConsole)))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Name < out[j].Name
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"stored_in": "tool_decls", "tools": out,
		// The vocabulary, so the console does not carry its own copy of a list
		// that lives in the domain model.
		"kinds":   evidenceKinds(),
		"effects": sideEffects(),
	})
}

// toolDeclInput is one save. Editable fields are pointers so that
// "not sent" and "cleared" are different requests.
//
// They have to be. The page has a dropdown per field and saves on change, so a
// a save may carry one field — and a struct that cannot tell absent from empty
// reads the others as "cleared" and writes the clear. Two quick changes then
// raced: each request rebuilt the whole entry from what it had, and the loser
// silently lost a field. Losing a side-effect level that way is not a cosmetic
// bug, because that level is what the gateway's ceiling policy reads.
type toolDeclInput struct {
	Name         string    `json:"name"`
	Server       string    `json:"server,omitempty"`
	Description  *string   `json:"description,omitempty"`
	UseCases     *[]string `json:"use_cases,omitempty"`
	Examples     *[]string `json:"examples,omitempty"`
	AntiExamples *[]string `json:"anti_examples,omitempty"`
	Suites       *[]string `json:"suites,omitempty"`
	Produces     *string   `json:"produces,omitempty"`
	Effect       *string   `json:"side_effect,omitempty"`
}

// handleSaveToolMetadata upserts one declaration and hot-reloads.
//
// An empty produces removes the declaration rather than storing a blank one:
// "not declared" and "declared as nothing" would look the same downstream, and
// only the first is true.
func (s *Server) handleSaveToolMetadata(w http.ResponseWriter, r *http.Request) {
	var in toolDeclInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	if in.Produces != nil && *in.Produces != "" && !validKind(*in.Produces) {
		writeErr(w, http.StatusBadRequest, "未知的 produces 取值："+*in.Produces)
		return
	}
	if in.Effect != nil && *in.Effect != "" && !ops.SideEffectLevel(*in.Effect).Valid() {
		writeErr(w, http.StatusBadRequest, "未知的 side_effect 取值："+*in.Effect)
		return
	}
	if in.Description == nil && in.UseCases == nil && in.Examples == nil &&
		in.AntiExamples == nil && in.Suites == nil && in.Produces == nil && in.Effect == nil {
		writeErr(w, http.StatusBadRequest, "没有要修改的字段")
		return
	}

	db, ok := s.stateDB(w)
	if !ok {
		return
	}

	// No mutex here any more, and that is the point.
	//
	// The lock this replaces guarded a file that every save rewrote in full,
	// and it only ever guarded it from this process — a second console, or a
	// second instance, raced it anyway. The row-per-tool table makes the
	// patch a single statement whose scope is one tool, and the transaction
	// around it is the database's, not one goroutine's idea of one.
	if err := toolreg.SaveDecl(r.Context(), db, toolreg.Decl{
		Name:   in.Name,
		Server: in.Server,
		// Backend is deliberately never set: it filters Registry.Available,
		// and a console declaration is about what a tool IS, not about which
		// incidents it belongs to — setting one here would hide the tool from
		// every view that asks without an incident.
		Description:  trimmed(in.Description),
		UseCases:     cleaned(in.UseCases),
		Examples:     cleaned(in.Examples),
		AntiExamples: cleaned(in.AntiExamples),
		Suites:       cleaned(in.Suites),
		Produces:     in.Produces,
		SideEffect:   in.Effect,
	}, s.engine().Config().Web.Admin.Username); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Only the registry is rebuilt. A config reload would cancel the MCP
	// context and kill every stdio subprocess — an absurd price for saying
	// that a tool returns metrics.
	s.engine().ReloadToolMetadata()
	// What the registry resolved goes back, not what was just written.
	//
	// They are not the same thing, because the declaration is a patch: a save
	// that sets only produces leaves the side effect to whatever a
	// hand-written file said, and answering with the console's own entry
	// reported that inherited field as 未声明. The page applies this response
	// directly — that is what keeps it from re-reading and racing the next
	// save — so the response has to be the same answer the list would give.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "saved_to": "tool_decls",
		"tool": s.effectiveDecl(r.Context(), db, in.Name, in.Server),
	})
}

// declRow is one row of the declarations table: what the registry resolved,
// where it came from, and whether the console's declaration survived.
//
// One definition, used by the list and by the save, because the page applies
// the save's answer straight into the list — two definitions would show a
// tool one way when saved and another way when reloaded.
func declRow(m, decl ops.ToolMetadata, declared bool, src string) toolDeclDTO {
	row := toolDeclDTO{
		Name: m.Name, Server: m.Server,
		Description: m.Description, UseCases: m.UseCases, Examples: m.Examples,
		AntiExamples: m.AntiExamples, Suites: m.Suites,
		Produces: string(m.Produces), Effect: string(m.SideEffect),
		Source: src,
	}
	if declared {
		row.Declared = &toolDeclFields{
			Description:  decl.Description,
			UseCases:     decl.UseCases,
			Examples:     decl.Examples,
			AntiExamples: decl.AntiExamples,
			Suites:       decl.Suites,
		}
		// Reported only about the fields the console actually declared.
		//
		// The declaration is a patch: it carries the field somebody set and
		// says nothing about the other one. Comparing both meant a tool
		// declared only for its produces read as 未生效 whenever a
		// hand-written file supplied its side effect — the two working
		// together, reported as one overriding the other. What is worth
		// reporting is a field the console set that the registry did not end
		// up with.
		row.Shadowed = (decl.Produces != "" && decl.Produces != m.Produces) ||
			(decl.SideEffect != "" && decl.SideEffect != m.SideEffect) ||
			(decl.Description != "" && decl.Description != m.Description) ||
			(len(decl.UseCases) > 0 && !slices.Equal(decl.UseCases, m.UseCases)) ||
			(len(decl.Examples) > 0 && !slices.Equal(decl.Examples, m.Examples)) ||
			(len(decl.AntiExamples) > 0 && !slices.Equal(decl.AntiExamples, m.AntiExamples)) ||
			(len(decl.Suites) > 0 && !slices.Equal(decl.Suites, m.Suites))
	}
	return row
}

// effectiveDecl reports what the registry now resolves for one tool.
//
// Called after a save, once the registry has been rebuilt. A tool the registry
// does not know — nothing else declares it and it is not connected — is
// answered with the declaration itself, which is exactly what will apply the
// moment it appears.
func (s *Server) effectiveDecl(ctx context.Context, db *storage.DB, name, server string) toolDeclDTO {
	// Read back rather than echo what was sent. A save carries a patch; what
	// the row now says is the patch applied to what was already there, and
	// the page shows the row.
	decl, declared := consoleDecl(ctx, db, server, name)
	if reg := s.engine().ToolRegistry(); reg != nil {
		if m, ok := reg.Lookup(name); ok && m.Server == server && m.Name == name {
			return declRow(m, decl, declared, sourceOf(m, declared))
		}
	}
	return declRow(decl, decl, declared, "console")
}

// consoleDecl reads one tool's row, or reports that there is none.
func consoleDecl(ctx context.Context, db *storage.DB, server, name string) (ops.ToolMetadata, bool) {
	metas, err := toolreg.NewDBSource(db).Load(ctx)
	if err != nil {
		return ops.ToolMetadata{}, false
	}
	for _, m := range metas {
		if m.Server == server && m.Name == name {
			return m, true
		}
	}
	return ops.ToolMetadata{}, false
}

// sourceOf names where a resolved entry's declaration came from, for the page.
func sourceOf(m ops.ToolMetadata, declaredInConsole bool) string {
	switch {
	case declaredInConsole:
		return "console"
	case m.Server != "":
		return "file"
	default:
		return "builtin"
	}
}

func declKey(server, name string) string { return server + "/" + name }

func hasConsoleFields(m ops.ToolMetadata) bool {
	return m.Description != "" || len(m.UseCases) > 0 || len(m.Examples) > 0 ||
		len(m.AntiExamples) > 0 || len(m.Suites) > 0 || m.Produces != "" || m.SideEffect != ""
}

func evidenceKinds() []map[string]string {
	return []map[string]string{
		{"value": string(ops.KindMetricSeries), "label": "监控指标"},
		{"value": string(ops.KindWorkloadStatus), "label": "运行状态"},
		{"value": string(ops.KindEvents), "label": "事件 / 告警"},
		{"value": string(ops.KindLogExcerpt), "label": "日志"},
		{"value": string(ops.KindTableRows), "label": "表格数据"},
		{"value": string(ops.KindConfig), "label": "配置"},
		{"value": string(ops.KindTopology), "label": "拓扑"},
		{"value": string(ops.KindKnowledge), "label": "知识 / 文档"},
		{"value": string(ops.KindText), "label": "文本"},
	}
}

func sideEffects() []map[string]string {
	return []map[string]string{
		{"value": string(ops.SideEffectReadOnly), "label": "只读"},
		{"value": string(ops.SideEffectMutating), "label": "会修改（可回滚）"},
		{"value": string(ops.SideEffectRisky), "label": "会修改（难回滚）"},
	}
}

func validKind(v string) bool {
	for _, k := range evidenceKinds() {
		if k["value"] == v {
			return true
		}
	}
	return false
}

// trimmed and cleaned normalise an optional field without losing the
// distinction the pointer carries: nil stays nil, because "said nothing" and
// "said empty" mean different things to SaveDecl.
func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

func cleaned(v *[]string) *[]string {
	if v == nil {
		return nil
	}
	c := cleanNames(*v)
	return &c
}
