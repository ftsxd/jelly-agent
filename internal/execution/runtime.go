package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

const DefaultTimeoutSec = 30
const DefaultMaxOutputKB = 64

type Request struct {
	Command    string `json:"command" jsonschema:"要运行的 CLI 命令；仅支持字面参数、引号、管道和逻辑连接，不支持变量展开、重定向或子 shell"`
	Purpose    string `json:"purpose" jsonschema:"执行目的，说明此查询如何帮助完成用户任务"`
	Profile    string `json:"profile" jsonschema:"执行配置名称，必须来自工具说明中该 Agent 已分配的配置"`
	TimeoutSec int    `json:"timeout_sec,omitempty" jsonschema:"可选超时秒数，只能收紧系统上限"`
}

type Observation struct {
	Evaluation
	ExecID           string `json:"exec_id"`
	Profile          string `json:"profile"`
	Executed         bool   `json:"executed"`
	ApprovalRequired bool   `json:"approval_required,omitempty"`
	ApprovalID       string `json:"approval_id,omitempty"`
	Approved         bool   `json:"approved,omitempty"`
	ExitCode         int    `json:"exit_code"`
	DurationMS       int64  `json:"duration_ms"`
	Stdout           string `json:"stdout,omitempty"`
	Stderr           string `json:"stderr,omitempty"`
	Truncated        bool   `json:"truncated"`
	TimedOut         bool   `json:"timed_out"`
	Cancelled        bool   `json:"cancelled,omitempty"`
	Backend          string `json:"backend,omitempty"`
	Error            string `json:"error,omitempty"`
}

type Runtime struct {
	Config    Config
	Approvals *Approvals
	// run is a package-private seam for asserting that denied calls never run.
	run func(context.Context, sandbox.Policy, sandbox.Spec) (sandbox.Result, error)
}

func (r Runtime) Check(agent string, req Request) Evaluation {
	if err := r.Config.Validate(); err != nil {
		return Evaluation{Decision: Forbidden, Reason: err.Error()}
	}
	if strings.TrimSpace(req.Purpose) == "" {
		return Evaluation{Decision: Forbidden, Reason: "执行目的不能为空"}
	}
	if req.TimeoutSec < 0 || req.TimeoutSec > 300 {
		return Evaluation{Decision: Forbidden, Reason: "请求超时必须为 0–300 秒"}
	}
	for _, p := range r.Config.ProfilesFor(agent) {
		if p.Name == req.Profile {
			return Evaluate(req.Command, p.Rules)
		}
	}
	return Evaluation{Decision: Forbidden, Reason: "执行器未启用，或当前 Agent 未获分配此执行配置"}
}

func (r Runtime) policy(profile Profile, req Request) sandbox.Policy {
	limit := r.Config.TimeoutSec
	if limit == 0 {
		limit = DefaultTimeoutSec
	}
	if req.TimeoutSec > 0 && req.TimeoutSec < limit {
		limit = req.TimeoutSec
	}
	cap := r.Config.MaxOutputKB
	if cap == 0 {
		cap = DefaultMaxOutputKB
	}
	backend := r.Config.Backend
	if backend == "" {
		backend = "os"
	}
	mode := sandbox.ModeWorkspace
	if profile.Network {
		mode = sandbox.ModeWorkspaceNet
	}
	return sandbox.Policy{Strict: true, Backend: backend, Mode: mode, Image: r.Config.Image,
		Timeout: time.Duration(limit) * time.Second, MaxOutput: cap << 10}
}

func (r Runtime) Execute(ctx context.Context, agent, session string, req Request) (out Observation) {
	return r.execute(ctx, agent, session, req, "")
}

// ExecuteApproved is called only on an authenticated confirmation resume.
// The store checks the context grant, exact args, identity, expiry and config.
func (r Runtime) ExecuteApproved(ctx context.Context, agent, session, call string, req Request) Observation {
	ev := r.Check(agent, req)
	if ev.Decision == Forbidden || !r.WritesEnabled(agent, req.Profile) || r.Approvals == nil {
		reason := "当前执行配置不允许审批执行"
		if ev.Decision == Forbidden {
			reason = ev.Reason
		}
		return Observation{Evaluation: Evaluation{Decision: Forbidden, Reason: reason}, Profile: req.Profile, ExitCode: -1, Error: reason}
	}
	id, err := r.Approvals.consume(ctx, r.Config, agent, session, call, req)
	if err != nil {
		return Observation{Evaluation: Evaluation{Decision: Forbidden, Reason: approvalError(err)}, Profile: req.Profile, ExitCode: -1, Error: approvalError(err)}
	}
	out := r.execute(ctx, agent, session, req, id)
	finishCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Approvals.Finish(finishCtx, id, out); err != nil {
		slog.Error("审批执行结果记录失败", "approval_id", id, "error", err)
	}
	return out
}

func (r Runtime) execute(ctx context.Context, agent, session string, req Request, approvedID string) (out Observation) {
	out = Observation{Evaluation: r.Check(agent, req), Profile: req.Profile, ExitCode: -1}
	out.ApprovalID, out.Approved = approvedID, approvedID != ""
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		out.Error = "生成执行标识失败"
		return
	}
	out.ExecID = "exec_" + hex.EncodeToString(id[:])
	defer func() {
		slog.InfoContext(ctx, "通用命令执行", "exec_id", out.ExecID, "session_id", session, "agent", agent,
			"approval_id", out.ApprovalID, "approved", out.Approved,
			"profile", out.Profile, "decision", out.Decision, "executed", out.Executed, "exit_code", out.ExitCode,
			"duration_ms", out.DurationMS, "truncated", out.Truncated)
	}()
	if out.Decision == Forbidden || (out.Decision != Allow && approvedID == "") {
		out.ApprovalRequired = out.Decision == Prompt
		return
	}
	if req.TimeoutSec < 0 || req.TimeoutSec > 300 {
		out.Decision = Forbidden
		out.Reason = "请求超时必须为 0–300 秒"
		return
	}
	var profile Profile
	for _, p := range r.Config.ProfilesFor(agent) {
		if p.Name == req.Profile {
			profile = p
			break
		}
	}
	if approvedID != "" {
		profile.Env = mergeEnv(profile.Env, profile.WriteEnv)
		if profile.WriteKubeconfigEnv != "" {
			profile.KubeconfigEnv = profile.WriteKubeconfigEnv
		}
	}
	env := map[string]string{}
	for key, source := range profile.Env {
		value, ok := os.LookupEnv(source)
		if !ok || value == "" {
			out.Error = "服务端未配置环境变量：" + source
			return
		}
		env[key] = value
	}
	pol := r.policy(profile, req)
	if err := sandbox.CheckStrict(pol); err != nil {
		out.Error = err.Error()
		return
	}
	// Each call gets a private workspace. Neither a model-provided cwd nor a
	// prior agent's generated file can change what this invocation executes.
	dir, err := os.MkdirTemp("", "jelly-exec-")
	if err != nil {
		out.Error = "创建执行工作目录失败"
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		os.RemoveAll(dir)
		out.Error = "无法保护执行工作目录"
		return
	}
	defer func() {
		defer root.Close()
		if err := cleanupWorkspace(root, dir); err != nil {
			slog.Error("执行凭据目录清理失败", "exec_id", out.ExecID, "error", err)
			if out.Error != "" {
				out.Error += "；"
			}
			out.Error += "执行工作目录清理失败，需要管理员处理；请勿重试命令"
		}
	}()
	var secrets map[string]string
	if profile.KubeconfigEnv != "" {
		secrets, err = materializeKubeconfig(profile.KubeconfigEnv, dir)
		if err != nil {
			out.Error = err.Error()
			return
		}
		env["KUBECONFIG"] = filepath.Join(dir, ".kube", "config")
		if pol.Backend == "docker" {
			env["KUBECONFIG"] = "/work/.kube/config"
		}
	}
	parsed, err := Parse(req.Command)
	if err != nil {
		out.Error = err.Error()
		return
	}
	argv := parsed.Segments[0]
	if len(parsed.Segments) > 1 {
		argv = []string{"bash", "--noprofile", "--norc", "-o", "pipefail", "-c", parsed.Script()}
	}
	run := r.run
	if run == nil {
		run = sandbox.Run
	}
	start := time.Now()
	res, err := run(ctx, pol, sandbox.Spec{Dir: dir, Argv: argv, Env: env, Secrets: secrets})
	out.DurationMS = time.Since(start).Milliseconds()
	out.Backend = res.Backend
	out.ExitCode = res.ExitCode
	out.TimedOut = res.TimedOut
	out.Cancelled = res.Cancelled || ctx.Err() == context.Canceled
	out.Truncated = res.Truncated
	// Executed distinguishes a policy/start failure from a child exiting 1.
	out.Executed = res.Started || err == nil
	for k, v := range secrets {
		env[k] = v
	} // redact-only after the child has ended
	out.Stdout = redact(res.Stdout, env, res.Truncated)
	out.Stderr = redact(res.Stderr, env, res.Truncated)
	if err != nil {
		stage := "命令启动失败"
		if out.Executed {
			stage = "命令已启动，但未能完整收尾；请勿自动重试"
		}
		out.Error = redact(fmt.Sprintf("%s：%s", stage, err), env, false)
	} else if ctx.Err() != nil {
		out.Error = "执行已取消：" + ctx.Err().Error()
	} else if out.TimedOut {
		out.Error = "执行超时；请核查目标资源，审批不会自动重试"
	} else if out.ExitCode != 0 {
		out.Error = fmt.Sprintf("命令退出码为 %d；请根据输出核查执行结果", out.ExitCode)
	}
	return
}

var secretField = regexp.MustCompile(`(?i)(["']?(?:secret[_-]?key|secret[_-]?id|access[_-]?token|refresh[_-]?token|api[_-]?key|password|authorization|token)["']?\s*[:=]\s*)("[^"\r\n]*"|'[^'\r\n]*'|(?:Bearer\s+)?[^\s,}\r\n]+)`)

func redact(text string, env map[string]string, truncated bool) string {
	// The sandbox masks injected values too. This second pass also covers
	// start errors and freshly issued token fields returned by an API.
	text = sandbox.Redact(text, env, truncated)
	text = secretField.ReplaceAllString(text, `${1}"[REDACTED]"`)
	return strings.ToValidUTF8(text, "�")
}

func (c Config) Instruction(agent string) string {
	profiles := c.ProfilesFor(agent)
	if len(profiles) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("通用执行器：优先使用已有结构化工具；缺少专用工具时可用 shell_exec 调用官方 CLI 或经显式规则授权的 SDK。先用 help 发现参数。不读取/打印凭据，不安装软件，不用额外权限绕过失败。prompt 命令只有启用了写操作审批的配置才能发起审批，系统等待用户在控制台确认原始命令后执行一次；自然语言同意不代表获批，不得改写命令或重复调用规避审批。forbidden 无法审批。每个审批十分钟失效，命令或配置改变必须重新申请。结果包含 exit_code、stdout/stderr 与 executed，executed=false 不是查询结果。结论引用网关返回的 evidence_id；已截断的输出不代表完整数据。凭据由系统注入，不向用户索要。\n可用执行配置：\n")
	for _, p := range profiles {
		fmt.Fprintf(&b, "- %s（联网：%t；可申请写审批：%t；变量名称：", p.Name, p.Network, p.WriteApproval)
		// Names, not source names or values. Sorting keeps prompts stable.
		keys := sortedKeys(p.Env)
		b.WriteString(strings.Join(keys, ", "))
		b.WriteString("）\n")
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
