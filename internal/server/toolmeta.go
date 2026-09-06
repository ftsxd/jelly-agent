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
)

// consoleFile is the file the console owns.
//
// Separate from anything hand-written so that saving from the UI can rewrite
// it whole without touching a file somebody maintains by hand — and so that
// deleting it undoes exactly what the console did.
const consoleFile = "console.yaml"

type metadataFile struct {
	Tools []ops.ToolMetadata `yaml:"tools"`
}

// toolDeclDTO is one tool's declaration as the console edits it.
//
// A deliberately small slice of ToolMetadata: these four are what the console
// can meaningfully ask about a third-party tool, and offering the rest would
// invite editing fields whose consequences are not visible from this page.
type toolDeclDTO struct {
	Name     string `json:"name"`
	Server   string `json:"server,omitempty"`
	Produces string `json:"produces,omitempty"`
	Effect   string `json:"side_effect,omitempty"`
	Source   string `json:"source,omitempty"` // "console" | "file" | "builtin"
}

// handleToolMetadata lists what has been declared, and where each came from.
func (s *Server) handleToolMetadata(w http.ResponseWriter, r *http.Request) {
	dir := s.engine().ToolMetadataDir()
	fromConsole := map[string]bool{}
	for _, m := range readDecls(filepath.Join(dir, consoleFile)) {
		fromConsole[declKey(m.Server, m.Name)] = true
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
			if fromConsole[declKey(m.Server, m.Name)] {
				src = "console"
			}
			out = append(out, toolDeclDTO{
				Name: m.Name, Server: m.Server,
				Produces: string(m.Produces), Effect: string(m.SideEffect), Source: src,
			})
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

type toolDeclInput struct {
	Name     string `json:"name"`
	Server   string `json:"server,omitempty"`
	Produces string `json:"produces,omitempty"`
	Effect   string `json:"side_effect,omitempty"`
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
	if in.Produces != "" && !validKind(in.Produces) {
		writeErr(w, http.StatusBadRequest, "未知的 produces 取值："+in.Produces)
		return
	}
	if in.Effect != "" && !ops.SideEffectLevel(in.Effect).Valid() {
		writeErr(w, http.StatusBadRequest, "未知的 side_effect 取值："+in.Effect)
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

	decls := readDecls(path)
	key := declKey(in.Server, in.Name)
	kept := decls[:0]
	for _, m := range decls {
		if declKey(m.Server, m.Name) != key {
			kept = append(kept, m)
		}
	}
	if in.Produces != "" || in.Effect != "" {
		kept = append(kept, ops.ToolMetadata{
			Name: in.Name, Server: in.Server,
			Produces:   ops.EvidenceKind(in.Produces),
			SideEffect: ops.SideEffectLevel(in.Effect),
			// Backend is deliberately left empty. It filters Registry.Available,
			// and a console declaration is about what a tool IS, not about
			// which incidents it belongs to — setting one here would hide the
			// tool from every view that asks without an incident.
		})
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
		"# 同目录下的其他 .yaml 文件不会被这里覆盖。\n"
	if err := os.WriteFile(path, append([]byte(header), body...), 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Only the registry is rebuilt. A config reload would cancel the MCP
	// context and kill every stdio subprocess — an absurd price for saying
	// that a tool returns metrics.
	s.engine().ReloadToolMetadata()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_to": path})
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
