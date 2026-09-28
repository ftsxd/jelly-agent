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
	OutcomeUnknown   bool   `json:"outcome_unknown,omitempty"`
	Outcome          string `json:"outcome,omitempty"`
	CleanupPending   bool   `json:"cleanup_pending,omitempty"`
	Error            string `json:"error,omitempty"`
}

type Runtime struct {
	Config    Config
	Approvals *Approvals
	Journal   *Journal
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
	return r.execute(ctx, agent, session, req, "", nil)
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
	var intent *RunRecord
	if r.Config.Backend == "docker" {
		if r.Journal == nil || r.Journal.DB != r.Approvals.DB {
			return Observation{Profile: req.Profile, ExitCode: -1, Error: "审批和执行恢复必须使用同一状态库，未消费审批"}
		}
		daemon, err := sandbox.DockerDaemonID(ctx)
		if err != nil {
			return Observation{Profile: req.Profile, ExitCode: -1, Error: err.Error()}
		}
		n, err := nonce()
		if err != nil {
			return Observation{Profile: req.Profile, ExitCode: -1, Error: "生成执行身份失败"}
		}
		var p Profile
		for _, candidate := range r.Config.ProfilesFor(agent) {
			if candidate.Name == req.Profile {
				p = candidate
				break
			}
		}
		authority, _ := ctx.Value(approvalKey{}).(approvalAuthority)
		record, err := r.Journal.Prepare("exec_"+n, agent, session, req.Profile, authority.id, daemon, r.policy(p, req).Timeout)
		if err != nil {
			return Observation{Profile: req.Profile, ExitCode: -1, Error: "无法准备持久化执行身份，未消费审批"}
		}
		intent = &record
	}
	id, err := r.Approvals.consumeIntent(ctx, r.Config, agent, session, call, req, intent)
	if err != nil {
		return Observation{Evaluation: Evaluation{Decision: Forbidden, Reason: approvalError(err)}, Profile: req.Profile, ExitCode: -1, Error: approvalError(err)}
	}
	out := r.execute(ctx, agent, session, req, id, intent)
	finishCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Approvals.Finish(finishCtx, id, out); err != nil {
		slog.Error("审批执行结果记录失败", "approval_id", id, "error", err)
	}
	return out
}

func (r Runtime) execute(ctx context.Context, agent, session string, req Request, approvedID string, intent *RunRecord) (out Observation) {
	out = Observation{Evaluation: r.Check(agent, req), Profile: req.Profile, ExitCode: -1}
	out.ApprovalID, out.Approved = approvedID, approvedID != ""
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		out.Error = "生成执行标识失败"
		return
	}
	out.ExecID = "exec_" + hex.EncodeToString(id[:])
	if intent != nil {
		out.ExecID = intent.ExecID
	}
	defer func() {
		if out.Outcome == "" {
			out.Outcome = observationOutcome(out)
		}
		slog.InfoContext(ctx, "通用命令执行", "exec_id", out.ExecID, "session_id", session, "agent", agent,
			"approval_id", out.ApprovalID, "approved", out.Approved,
			"profile", out.Profile, "decision", out.Decision, "executed", out.Executed, "exit_code", out.ExitCode,
			"duration_ms", out.DurationMS, "truncated", out.Truncated)
	}()
	if intent != nil {
		defer r.finishManaged(&out, *intent)
	}
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
		profile.AgentEnv = mergeEnv(profile.AgentEnv, profile.WriteAgentEnv)
		for key := range profile.WriteEnv {
			delete(profile.AgentEnv, key)
		}
		for key := range profile.WriteAgentEnv {
			delete(profile.Env, key)
		}
		if profile.WriteKubeconfigEnv != "" {
			profile.KubeconfigEnv = profile.WriteKubeconfigEnv
		}
	}
	env := map[string]string{}
	// Resolve the read identity, then overlay approved write sources across
	// both source types. No other agent's variables or ambient secrets enter.
	inject := func(mapping map[string]string, values map[string]string, agentSource bool) bool {
		for key, source := range mapping {
			value, ok := os.LookupEnv(source)
			if agentSource {
				value, ok = values[source]
			}
			if !ok || value == "" {
				out.Error = "未配置执行变量来源：" + source
				return false
			}
			env[key] = value
		}
		return true
	}
	if !inject(profile.Env, nil, false) || !inject(profile.AgentEnv, r.Config.agentVars[agent], true) {
		return
	}
	if profile.ToolDir != "" {
		if st, err := os.Stat(filepath.Join(profile.ToolDir, "bin")); err != nil || !st.IsDir() {
			out.Error = "工具运行目录不可用，请管理员安装所需 CLI"
			return
		}
		// PATH is administrator-controlled runtime wiring, never a profile
		// variable or model argument. HOME remains the private workspace.
		env["PATH"] = filepath.Join(profile.ToolDir, "bin") + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	pol := r.policy(profile, req)
	if profile.ToolDir != "" {
		var err error
		pol.ReadPaths, err = trustedToolPaths(profile.ToolDir)
		if err != nil {
			out.Error = err.Error()
			return
		}
	}
	if err := sandbox.CheckStrict(pol); err != nil {
		out.Error = err.Error()
		return
	}
	// Each call gets a private workspace. Neither a model-provided cwd nor a
	// prior agent's generated file can change what this invocation executes.
	var dir string
	var err error
	var managed *sandbox.ManagedContainer
	if pol.Backend == "docker" {
		if r.Journal == nil {
			out.Error = "容器执行需要持久化恢复记录，未启动命令"
			return
		}
		daemon, e := sandbox.DockerDaemonID(ctx)
		if e != nil {
			out.Error = e.Error()
			return
		}
		var record RunRecord
		var workspace string
		if intent != nil {
			record = *intent
			if record.Daemon != daemon {
				out.Error = "Docker 服务身份已改变，命令未启动"
				return
			}
			workspace, e = r.Journal.Workspace(ctx, record)
		} else {
			record, workspace, e = r.Journal.Begin(ctx, out.ExecID, agent, session, req.Profile, approvedID, daemon, pol.Timeout)
			if record.ExecID != "" {
				defer r.finishManaged(&out, record)
			}
		}
		if e != nil {
			out.Error = "无法持久化执行身份或创建私有目录，未启动命令"
			return
		}
		dir = workspace
		managed = &sandbox.ManagedContainer{Identity: identity(record),
			Created:  func(cid string) error { return r.Journal.Created(ctx, record, cid) },
			Starting: func() error { return r.Journal.Starting(ctx, record, pol.Timeout) }}
	} else {
		dir, err = os.MkdirTemp("", "jelly-exec-")
	}
	if err != nil {
		out.Error = "创建执行工作目录失败"
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		if managed == nil {
			os.RemoveAll(dir)
		}
		out.Error = "无法保护执行工作目录"
		return
	}
	defer func() {
		defer root.Close()
		if managed != nil {
			return
		} // Container termination must precede credentials removal.
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
	if profile.ToolDir != "" {
		candidate := filepath.Join(profile.ToolDir, "bin", argv[0])
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0111 != 0 {
			argv = append([]string{candidate}, argv[1:]...)
		}
	}
	if len(parsed.Segments) > 1 {
		argv = []string{"bash", "--noprofile", "--norc", "-o", "pipefail", "-c", parsed.Script()}
	}
	run := r.run
	if run == nil {
		run = sandbox.Run
	}
	start := time.Now()
	res, err := run(ctx, pol, sandbox.Spec{Dir: dir, Argv: argv, Env: env, Secrets: secrets, Managed: managed})
	out.DurationMS = time.Since(start).Milliseconds()
	out.Backend = res.Backend
	out.ExitCode = res.ExitCode
	out.TimedOut = res.TimedOut
	out.Cancelled = res.Cancelled || ctx.Err() == context.Canceled
	out.Truncated = res.Truncated
	// Executed distinguishes a policy/start failure from a child exiting 1.
	out.Executed = res.Started || err == nil
	if managed != nil {
		out.Executed = res.Started
	}
	out.OutcomeUnknown = err != nil && res.Started
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
	out.Outcome = observationOutcome(out)
	return
}

func observationOutcome(out Observation) string {
	if out.Outcome != "" {
		return out.Outcome
	}
	switch {
	case out.OutcomeUnknown:
		return "unknown"
	case out.TimedOut:
		return "timed_out"
	case out.Cancelled:
		return "cancelled"
	case out.Error != "" || !out.Executed || out.ExitCode != 0:
		return "failed"
	default:
		return "succeeded"
	}
}

func (r Runtime) finishManaged(out *Observation, record RunRecord) {
	out.Outcome = observationOutcome(*out)
	finish, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	finishErr := r.Journal.Finish(finish, record, *out)
	if finishErr != nil {
		if saved, err := r.Journal.Get(finish, record.ExecID); err == nil && saved.State == "unknown" {
			out.OutcomeUnknown = true
			out.Outcome = "unknown"
		}
		out.Error += "；执行终态未按本次调用确认，请核查资源；禁止自动重试"
	}
	cancel()
	cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
	cleanupErr := r.Journal.Cleanup(cleanup, record.ExecID)
	done()
	if cleanupErr != nil {
		out.CleanupPending = true
		out.Error += "；资源回收未确认，已保留恢复记录；禁止自动重试"
	}
	if finishErr != nil || cleanupErr != nil {
		slog.Error("执行资源收尾失败", "exec_id", record.ExecID)
	}
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
	b.WriteString("通用执行器：用户要求查询或排查时，要实际调用工具完成任务。优先使用已有结构化工具；缺少专用工具时，用 shell_exec 调用官方 CLI 或经显式规则授权的 SDK。参数不确定先调用 help，拿到帮助后立即继续只读查询；根据真实 stderr 修正参数并重试合法的只读查询，必要时分页直至结果完整。不能仅返回脚本让用户自己运行，也不能在未尝试已分配工具时声称没有执行能力。缺凭据、权限不足或 CLI 不可用时，引用实际失败证据说明阻塞，不虚构资源或绕过限制。不读取/打印凭据，不安装软件，不用额外权限绕过失败。prompt 命令只有启用了写操作审批的配置才能发起审批，系统等待用户在控制台确认原始命令后执行一次；自然语言同意不代表获批，不得改写命令或重复调用规避审批。forbidden 无法审批。每个审批十分钟失效，命令或配置改变必须重新申请。结果包含 exit_code、stdout/stderr 与 executed，executed=false 不是查询结果。结论引用网关返回的 evidence_id；已截断的输出不代表完整数据。凭据由系统注入，不向用户索要。\n")
	b.WriteString("输出被截断时，先查 CLI 顶层帮助寻找原生字段过滤与分页能力，以所需字段和总数精简输出。tccli 支持 --filter（JMESPath，本地过滤返回值），可保留 TotalCount、RequestId 和所需资源字段；避免依赖未获授权的本地脚本或处理器。遇到权限拒绝时，不尝试改变身份或扩大资源范围。\n可用执行配置：\n")
	for _, p := range profiles {
		fmt.Fprintf(&b, "- %s（联网：%t；可申请写审批：%t；变量名称：", p.Name, p.Network, p.WriteApproval)
		// Names, not source names or values. Sorting keeps prompts stable.
		keys := sortedKeys(mergeEnv(p.Env, p.AgentEnv))
		b.WriteString(strings.Join(keys, ", "))
		b.WriteString("）\n")
	}
	return b.String()
}

func trustedToolPaths(dir string) ([]string, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("工具运行目录不可用")
	}
	var reads []string
	for _, part := range []string{"bin", "lib", "lib64", "pyvenv.cfg"} {
		path := filepath.Join(root, part)
		resolved, err := filepath.EvalSymlinks(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("无法核验工具运行目录")
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("工具运行目录的 %s 链接指向目录外，不能扩大读取范围", part)
		}
		reads = append(reads, resolved)
	}
	return reads, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
