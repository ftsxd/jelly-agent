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
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/toolreg"
)

// consoleFile is the file the console owns.
//
// Separate from anything hand-written so that saving from the UI can rewrite
// it whole without touching a file somebody maintains by hand — and so that
// deleting it undoes exactly what the console did. The name lives in toolreg
// because that is what loads it first, which is what makes a declaration made
// here actually take effect rather than lose to a file that sorts earlier.
const consoleFile = toolreg.ConsoleFile

type metadataFile struct {
	Tools []ops.ToolMetadata `yaml:"tools"`
}

// toolDeclDTO is one tool's declaration as the console edits it.
//
// A deliberately small slice of ToolMetadata: these four are what the console
// can meaningfully ask about a third-party tool, and offering the rest would
// invite editing fields whose consequences are not visible from this page.
type toolDeclDTO struct {
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
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

// handleToolMetadata lists what has been declared, and where each came from.
func (s *Server) handleToolMetadata(w http.ResponseWriter, r *http.Request) {
	dir := s.engine().ToolMetadataDir()
	fromConsole := map[string]ops.ToolMetadata{}
	for _, m := range readDecls(filepath.Join(dir, consoleFile)) {
		fromConsole[declKey(m.Server, m.Name)] = m
	}

	out := []toolDeclDTO{}
	if reg := s.engine().ToolRegistry(); reg != nil {
		for _, name := range reg.Names() {
			m, ok := reg.Lookup(name)
			if !ok || m.Name != name {
				continue // aliases resolve to the same entry; list it once
			}
			src := "builtin"
			if m.Server != "" {
				src = "file"
			}
			decl, declared := fromConsole[declKey(m.Server, m.Name)]
			if declared {
				src = "console"
			}
			out = append(out, declRow(m, decl, declared, src))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Name < out[j].Name
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"dir": dir, "file": consoleFile, "tools": out,
		// The vocabulary, so the console does not carry its own copy of a list
		// that lives in the domain model.
		"kinds":   evidenceKinds(),
		"effects": sideEffects(),
	})
}

// toolDeclInput is one save. The two editable fields are pointers so that
// "not sent" and "cleared" are different requests.
//
// They have to be. The page has a dropdown per field and saves on change, so a
// save carries one field — and a struct that cannot tell absent from empty
// read the other one as "cleared" and wrote the clear. Two quick changes then
// raced: each request rebuilt the whole entry from what it had, and the loser
// silently lost a field. Losing a side-effect level that way is not a cosmetic
// bug, because that level is what the gateway's ceiling policy reads.
type toolDeclInput struct {
	Name     string  `json:"name"`
	Server   string  `json:"server,omitempty"`
	Produces *string `json:"produces,omitempty"`
	Effect   *string `json:"side_effect,omitempty"`
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
	if in.Produces == nil && in.Effect == nil {
		writeErr(w, http.StatusBadRequest, "没有要修改的字段")
		return
	}

	dir := s.engine().ToolMetadataDir()
	if dir == "" {
		writeErr(w, http.StatusInternalServerError, "无法确定工具元数据目录")
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := filepath.Join(dir, consoleFile)

	// One save at a time. Every save is a read-modify-write of one file, and
	// the page issues them a field at a time — two dropdowns changed quickly
	// both read the same old file and the second one wrote over the first.
	// Serialising here rather than in the browser because the file is what is
	// being protected, and a second console would race the first anyway.
	s.declMu.Lock()
	defer s.declMu.Unlock()

	decls := readDecls(path)
	key := declKey(in.Server, in.Name)
	cur := ops.ToolMetadata{Name: in.Name, Server: in.Server}
	kept := make([]ops.ToolMetadata, 0, len(decls)+1)
	for _, m := range decls {
		if declKey(m.Server, m.Name) == key {
			cur = m // start from what is on disk now, not from what the page had
			continue
		}
		kept = append(kept, m)
	}
	// Only the fields this request carried. Backend is deliberately never set:
	// it filters Registry.Available, and a console declaration is about what a
	// tool IS, not about which incidents it belongs to — setting one here
	// would hide the tool from every view that asks without an incident.
	cur.Name, cur.Server, cur.Backend = in.Name, in.Server, ""
	if in.Produces != nil {
		cur.Produces = ops.EvidenceKind(*in.Produces)
	}
	if in.Effect != nil {
		cur.SideEffect = ops.SideEffectLevel(*in.Effect)
	}
	// An entry with nothing left in it is removed rather than stored blank:
	// "not declared" and "declared as nothing" look the same downstream, and
	// only the first is true.
	if cur.Produces != "" || cur.SideEffect != "" {
		kept = append(kept, cur)
	}
	sort.Slice(kept, func(i, j int) bool {
		return declKey(kept[i].Server, kept[i].Name) < declKey(kept[j].Server, kept[j].Name)
	})

	body, err := yaml.Marshal(metadataFile{Tools: kept})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	header := "# 由控制台维护，可以手工编辑。\n" +
		"# 同目录下的其他 .yaml 文件不会被这里覆盖；这个文件优先于它们生效。\n"
	if err := writeFileAtomic(path, append([]byte(header), body...)); err != nil {
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
		"ok": true, "saved_to": path,
		"tool": s.effectiveDecl(in.Name, in.Server, cur),
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
		Produces: string(m.Produces), Effect: string(m.SideEffect),
		Source: src,
	}
	if declared {
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
			(decl.SideEffect != "" && decl.SideEffect != m.SideEffect)
	}
	return row
}

// effectiveDecl reports what the registry now resolves for one tool.
//
// Called after a save, once the registry has been rebuilt. A tool the registry
// does not know — nothing else declares it and it is not connected — is
// answered with the declaration itself, which is exactly what will apply the
// moment it appears.
func (s *Server) effectiveDecl(name, server string, decl ops.ToolMetadata) toolDeclDTO {
	if reg := s.engine().ToolRegistry(); reg != nil {
		if m, ok := reg.Lookup(name); ok && m.Server == server && m.Name == name {
			return declRow(m, decl, true, "console")
		}
	}
	return declRow(decl, decl, true, "console")
}

// writeFileAtomic replaces a file in one step.
//
// os.WriteFile truncates and then writes, so a reader arriving in between — the
// registry's own file watcher, or the next save's read-modify-write — sees an
// empty or half-written file and treats it as "nothing declared". Writing a
// temporary file in the same directory and renaming it over the target makes
// the replacement a single operation as far as any reader is concerned.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once the rename has succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func declKey(server, name string) string { return server + "/" + name }

// readDecls loads the console's file, treating any problem as "nothing
// declared yet" — the file is ours, a missing one is the normal first state,
// and a corrupt one must not stop the page from working.
func readDecls(path string) []ops.ToolMetadata {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f metadataFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil
	}
	return f.Tools
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
