package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
)

// The page answers "what do we inject", and it used to answer for the built-in
// constant whatever was actually running. A deployment with a coordinator that
// has its own instruction was shown a prompt the model never receives, under a
// heading that says 发给模型的原文.
func TestThePromptPageShowsTheRunningAgentsInstruction(t *testing.T) {
	s := newTestServer(t)
	cfg := s.engine().Config()
	cfg.Agents = []config.AgentDef{
		{Name: "Orchestrator", Instruction: "你是协调者，先判断该转交给谁。", Enabled: true},
		{Name: "Inherits", Enabled: true},
	}
	cfg.DefaultAgent = "Orchestrator"

	// No agent named: the server falls back to default_agent, so the page opens
	// on what a turn would actually use.
	if got := instructionFromAPI(t, s, ""); got != "你是协调者，先判断该转交给谁。" {
		t.Errorf("默认打开显示的是 %q，应当是默认 agent 的指令", got)
	}
	// An agent with no instruction of its own inherits the base.
	if got := instructionFromAPI(t, s, "?agent=Inherits"); got != engine.RootInstruction {
		t.Errorf("没有自己指令的 agent 显示 %q，应当沿用基础指令", got)
	}
}

// The base instruction is editable, because the built-in text describes a
// general assistant and a deployment that is an ops-diagnosis agent has to be
// able to say so without rebuilding a binary.
func TestTheBaseInstructionIsEditable(t *testing.T) {
	s := newTestServer(t)
	// SourcePath, not WithConfigPath: writeTargetPath resolves the file to edit
	// from the running config's own source, and falls back to the real
	// ~/.jelly-agent/config.yaml when that is empty. Setting only the reload
	// path let this test write into the developer's own config — it did, once.
	path := filepath.Join(t.TempDir(), "config.yaml")
	s.engine().Config().SourcePath = path
	s = s.WithConfigPath(path)

	const mine = "你是运维诊断 agent。先定位时间窗，再取证据，最后给结论。"
	if w := do(t, s, "PUT", "/api/prompt/instruction",
		`{"instruction":"`+mine+`"}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := s.engine().BaseInstruction(); got != mine {
		t.Errorf("引擎读到的是 %q", got)
	}
	if got := instructionFromAPI(t, s, ""); got != mine {
		t.Errorf("页面读回的是 %q", got)
	}

	// Clearing it restores the built-in — the only way back once something has
	// been written.
	if w := do(t, s, "PUT", "/api/prompt/instruction", `{"instruction":"   "}`); w.Code != http.StatusOK {
		t.Fatalf("clear: status = %d: %s", w.Code, w.Body.String())
	}
	if got := s.engine().BaseInstruction(); got != engine.RootInstruction {
		t.Errorf("清空后没有恢复内置默认: %q", got)
	}
}

func instructionFromAPI(t *testing.T, s *Server, query string) string {
	t.Helper()
	w := do(t, s, "GET", "/api/prompt"+query, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Parts []struct {
			Name string `json:"name"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, p := range body.Parts {
		if p.Name == "指令" {
			return p.Text
		}
	}
	t.Fatal("响应里没有「指令」这一块")
	return ""
}

// 未声明的 MCP 工具，副作用等级也必须解析出来。
//
// 快照路径原来只在注册表里查得到时才设这个字段，而 42 个 n9e 工具里只有 3 个
// 声明过——剩下 39 个是空串，前端那个 badge 又是无条件渲染的，于是显示成空白。
// 而空白读起来像"没有副作用"，恰恰是策略里写明不能这么读的：第三方服务器
// 沉默不等于安全保证，远端工具没声明就按会改动处理。
func TestAnUndeclaredRemoteToolStillReportsItsSideEffect(t *testing.T) {
	s := newTestServer(t)
	// 一次实发观测，里面有一个注册表里查不到的远端工具。
	s.engine().SetPromptSnapshotForTest("root", engine.PromptSnapshot{
		ToolsTokens: 120,
		At:          time.Now(),
		Tools: []engine.PromptToolSnapshot{
			{Name: "list_notify_channels", Description: "List notification channels.",
				Tokens: 60, Server: "n9e-mcp"},
			{Name: "web_search", Description: "Search the web.", Tokens: 60},
		},
	})

	w := do(t, s, "GET", "/api/prompt", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Measured   bool  `json:"tools_measured"`
		MeasuredAt int64 `json:"tools_measured_at"`
		Tools      []struct {
			Name       string `json:"name"`
			Server     string `json:"server"`
			SideEffect string `json:"side_effect"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Measured {
		t.Fatal("有观测样本却报成按元数据估算")
	}
	if body.MeasuredAt == 0 {
		t.Error("没有给出观测时间——页面就无法说明这份测量是何时的")
	}
	byName := map[string]string{}
	server := map[string]string{}
	for _, tl := range body.Tools {
		byName[tl.Name] = tl.SideEffect
		server[tl.Name] = tl.Server
	}
	// 远端且未声明 → 按会改动处理，绝不能是空串。
	if got := byName["list_notify_channels"]; got != "mutating" {
		t.Errorf("未声明的远端工具副作用 = %q，应当按 mutating 处理", got)
	}
	if server["list_notify_channels"] != "n9e-mcp" {
		t.Errorf("快照没把来源服务器带过来: %q", server["list_notify_channels"])
	}
	// 内置且未声明 → 只读，这是另一半，不能一起改成 mutating。
	if got := byName["web_search"]; got != "read_only" {
		t.Errorf("未声明的内置工具副作用 = %q，应当是 read_only", got)
	}
}
