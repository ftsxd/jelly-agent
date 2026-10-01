// Package execution implements the opt-in generic diagnostic executor.
package execution

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

type Decision string

const (
	Allow     Decision = "allow"
	Prompt    Decision = "prompt"
	Forbidden Decision = "forbidden"
)

type Rule struct {
	Name     string   `json:"name" yaml:"name"`
	Pattern  []string `json:"pattern" yaml:"pattern"`
	Decision Decision `json:"decision" yaml:"decision"`
	Reason   string   `json:"reason,omitempty" yaml:"reason,omitempty"`
}

type Profile struct {
	Name   string   `json:"name" yaml:"name"`
	Agents []string `json:"agents" yaml:"agents"`
	// Env maps a child variable to a server environment variable NAME, not a value.
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	// AgentEnv maps child variables to this agent's saved variable NAMES.
	// Values are resolved by the server, never supplied by a model argument.
	AgentEnv map[string]string `json:"agent_env,omitempty" yaml:"agent_env,omitempty"`
	// ToolDir is an administrator-installed runtime. Only its bin/lib/lib64
	// and pyvenv.cfg are readable; it never grants access to the host HOME.
	ToolDir string `json:"tool_dir,omitempty" yaml:"tool_dir,omitempty"`
	// KubeconfigEnv names a server variable containing an inline read-only
	// kubeconfig. It is materialized privately, never a host path mount.
	KubeconfigEnv string `json:"kubeconfig_env,omitempty" yaml:"kubeconfig_env,omitempty"`
	Network       bool   `json:"network" yaml:"network,omitempty"`
	Rules         []Rule `json:"rules,omitempty" yaml:"rules,omitempty"`
	// Writes stay opt-in. Elevated sources are injected only after one-use approval.
	WriteApproval      bool              `json:"write_approval,omitempty" yaml:"write_approval,omitempty"`
	WriteEnv           map[string]string `json:"write_env,omitempty" yaml:"write_env,omitempty"`
	WriteAgentEnv      map[string]string `json:"write_agent_env,omitempty" yaml:"write_agent_env,omitempty"`
	WriteKubeconfigEnv string            `json:"write_kubeconfig_env,omitempty" yaml:"write_kubeconfig_env,omitempty"`
	// AllowUnconfinedWithApproval is the last-resort escape hatch for a host
	// where docker, bwrap and Landlock are all unavailable: an otherwise
	// Allow-decided call is escalated to Prompt instead of hard-failing, and
	// only after a human approves it does the call run with no sandbox at all
	// (Observation.Unconfined records that it happened). Default false — an
	// unset profile keeps failing closed exactly as before this existed.
	AllowUnconfinedWithApproval bool `json:"allow_unconfined_with_approval,omitempty" yaml:"allow_unconfined_with_approval,omitempty"`
	// InheritAgentVars injects each assigned agent's saved variables under
	// their own names, so the common case needs no AgentEnv at all. Unset
	// means on; false keeps only the explicit mappings.
	InheritAgentVars *bool `json:"inherit_agent_vars,omitempty" yaml:"inherit_agent_vars,omitempty"`
}

// InheritsAgentVars reports whether the profile adds same-name agent variables.
func (p Profile) InheritsAgentVars() bool { return p.InheritAgentVars == nil || *p.InheritAgentVars }

type Config struct {
	Enabled     bool      `json:"enabled" yaml:"enabled,omitempty"`
	Backend     string    `json:"backend" yaml:"backend,omitempty"`
	Image       string    `json:"image,omitempty" yaml:"image,omitempty"`
	TimeoutSec  int       `json:"timeout_sec" yaml:"timeout_sec,omitempty"`
	MaxOutputKB int       `json:"max_output_kb" yaml:"max_output_kb,omitempty"`
	Profiles    []Profile `json:"profiles" yaml:"profiles,omitempty"`
	agentVars   map[string]map[string]string
}

// WithAgentVars takes a private snapshot; JSON/YAML and tool schemas expose
// only mappings, never these values. Snapshotting also binds pending approval
// to the identity used when the agent was built.
func (c Config) WithAgentVars(vars map[string]map[string]string) Config {
	c.agentVars = make(map[string]map[string]string)
	for _, p := range c.Profiles {
		for _, agent := range p.Agents {
			if c.agentVars[agent] == nil {
				c.agentVars[agent] = make(map[string]string)
			}
			if p.InheritsAgentVars() {
				for source, value := range vars[agent] {
					c.agentVars[agent][source] = value
				}
			}
			for _, mapping := range []map[string]string{p.AgentEnv, p.WriteAgentEnv} {
				for _, source := range mapping {
					if value, ok := vars[agent][source]; ok {
						c.agentVars[agent][source] = value
					}
				}
			}
		}
	}
	return c
}

// inheritedAgentEnv is the same-name mapping an inheriting profile adds for
// agent. It skips what must not or need not be injected this way: sources
// reserved for approved runs, names that would change the execution
// environment, empty values, and anything the profile already maps
// explicitly, as a child variable or as a source.
func (c Config) inheritedAgentEnv(p Profile, agent string) map[string]string {
	out := map[string]string{}
	if !p.InheritsAgentVars() {
		return out
	}
	reserved := c.ApprovalVarsFor(agent)
	explicit := map[string]bool{}
	for _, mapping := range []map[string]string{p.Env, p.AgentEnv} {
		for key, source := range mapping {
			explicit[key] = true
			explicit[source] = true
		}
	}
	for name, value := range c.agentVars[agent] {
		if value == "" || reserved[name] || explicit[name] || !envName.MatchString(name) || dangerousEnv(name) {
			continue
		}
		out[name] = name
	}
	return out
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c Config) Validate() error {
	if c.Backend != "" && c.Backend != "os" && c.Backend != "docker" {
		return fmt.Errorf("执行后端只能是 os 或 docker")
	}
	if c.TimeoutSec < 0 || c.TimeoutSec > 300 {
		return fmt.Errorf("执行超时必须为 0–300 秒")
	}
	if c.MaxOutputKB < 0 || c.MaxOutputKB > 1024 {
		return fmt.Errorf("输出上限必须为 0–1024 KiB")
	}
	if c.Enabled && len(c.Profiles) == 0 {
		return fmt.Errorf("启用执行器需要至少一个执行配置")
	}
	seen := map[string]bool{}
	for _, p := range c.Profiles {
		if c.Backend == "docker" && p.ToolDir != "" {
			return fmt.Errorf("Docker 的 CLI 必须预装在镜像中；工具运行目录仅适用于系统沙箱")
		}
		if !identifier.MatchString(p.Name) || seen[p.Name] {
			return fmt.Errorf("执行配置名称无效或重复：%s", p.Name)
		}
		seen[p.Name] = true
		if len(p.Agents) == 0 {
			return fmt.Errorf("%s 必须明确分配给至少一个 Agent", p.Name)
		}
		for _, a := range p.Agents {
			if !identifier.MatchString(a) {
				return fmt.Errorf("Agent 名称无效：%s", a)
			}
		}
		for _, variables := range []map[string]string{p.Env, p.WriteEnv, p.AgentEnv, p.WriteAgentEnv} {
			for k, source := range variables {
				if !envName.MatchString(k) || !envName.MatchString(source) || dangerousEnv(k) {
					return fmt.Errorf("环境变量映射无效或会改变执行环境：%s", k)
				}
			}
		}
		for _, pair := range [][2]map[string]string{{p.Env, p.AgentEnv}, {p.WriteEnv, p.WriteAgentEnv}} {
			for key := range pair[0] {
				if _, ok := pair[1][key]; ok {
					return fmt.Errorf("%s 的变量 %s 同时引用服务端和 Agent 来源", p.Name, key)
				}
			}
		}
		if p.ToolDir != "" && (!filepath.IsAbs(p.ToolDir) || filepath.Clean(p.ToolDir) != p.ToolDir || p.ToolDir == string(filepath.Separator) || strings.ContainsAny(p.ToolDir, "\x00\r\n") || c.Backend == "docker") {
			return fmt.Errorf("%s 的工具运行目录必须为系统沙箱中的独立绝对目录", p.Name)
		}
		if (p.KubeconfigEnv != "" && !envName.MatchString(p.KubeconfigEnv)) || (p.WriteKubeconfigEnv != "" && !envName.MatchString(p.WriteKubeconfigEnv)) {
			return fmt.Errorf("kubeconfig 必须引用服务端环境变量名称")
		}
		if !p.WriteApproval && (len(p.WriteEnv) > 0 || len(p.WriteAgentEnv) > 0 || p.WriteKubeconfigEnv != "") {
			return fmt.Errorf("%s 的写凭据需要先启用写操作审批", p.Name)
		}
		rules := map[string]bool{}
		for _, r := range p.Rules {
			if r.Name == "" || rules[r.Name] {
				return fmt.Errorf("规则名称为空或重复：%s", r.Name)
			}
			rules[r.Name] = true
			if r.Decision != Allow && r.Decision != Prompt && r.Decision != Forbidden {
				return fmt.Errorf("规则 %s 决策无效", r.Name)
			}
			if len(r.Pattern) == 0 || !identifier.MatchString(r.Pattern[0]) {
				return fmt.Errorf("规则 %s 需要命令 token 前缀，不能使用路径或 shell", r.Name)
			}
			for _, token := range r.Pattern {
				if token == "" || strings.ContainsAny(token, "\x00\r\n") {
					return fmt.Errorf("规则 %s 含无效 token", r.Name)
				}
			}
		}
	}
	for _, p := range c.Profiles {
		for _, agent := range p.Agents {
			reserved := c.ApprovalVarsFor(agent)
			for _, source := range p.AgentEnv {
				if reserved[source] {
					return fmt.Errorf("%s 的 Agent 变量 %s 已保留给审批执行，不能同时用于自动诊断", agent, source)
				}
			}
		}
	}
	return nil
}

// ApprovalVarsFor reserves sources even while the executor is disabled.
// Disabling execution must not expose elevated variables to skill scripts.
func (c Config) ApprovalVarsFor(agent string) map[string]bool {
	out := map[string]bool{}
	for _, p := range c.Profiles {
		for _, assigned := range p.Agents {
			if assigned == agent {
				for _, source := range p.WriteAgentEnv {
					out[source] = true
				}
				break
			}
		}
	}
	return out
}

func dangerousEnv(k string) bool {
	for _, x := range []string{"PATH", "HOME", "TMPDIR", "ENV", "BASH_ENV", "SHELLOPTS", "BASHOPTS", "CDPATH", "IFS", "PYTHONPATH", "PYTHONHOME", "NODE_OPTIONS", "PERL5OPT", "RUBYOPT", "GIT_CONFIG", "GIT_CONFIG_COUNT", "KUBECONFIG"} {
		if k == x {
			return true
		}
	}
	return strings.HasPrefix(k, "LD_") || strings.HasPrefix(k, "DYLD_") || strings.HasPrefix(k, "BASH_FUNC_") || strings.HasPrefix(k, "DOCKER_")
}

func (c Config) ProfilesFor(agent string) []Profile {
	var out []Profile
	if !c.Enabled {
		return out
	}
	for _, p := range c.Profiles {
		for _, a := range p.Agents {
			if a == agent {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func DefaultRules() []Rule {
	var rules []Rule
	for _, v := range []string{"get", "describe", "logs", "top", "version", "api-resources", "api-versions", "help"} {
		rules = append(rules, Rule{Name: "kubectl-" + v, Pattern: []string{"kubectl", v}, Decision: Allow})
	}
	for _, v := range []string{"apply", "patch", "scale", "rollout", "create", "edit", "exec", "cp", "replace", "label", "annotate", "cordon", "drain"} {
		rules = append(rules, Rule{Name: "kubectl-change-" + v, Pattern: []string{"kubectl", v}, Decision: Prompt, Reason: "变更或远程执行需要逐次审批"})
	}
	rules = append(rules, Rule{Name: "tccli-help", Pattern: []string{"tccli", "help"}, Decision: Allow})
	rules = append(rules, Rule{Name: "tccli-version", Pattern: []string{"tccli", "--version"}, Decision: Allow})
	return rules
}

type Evaluation struct {
	Decision     Decision   `json:"decision"`
	Reason       string     `json:"reason,omitempty"`
	Segments     [][]string `json:"segments,omitempty"`
	MatchedRules []string   `json:"matched_rules,omitempty"`
}

func strictest(a, b Decision) Decision {
	rank := map[Decision]int{Allow: 0, Prompt: 1, Forbidden: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// Evaluate checks EVERY segment before any segment is executed. Rules cannot
// authorize shell substitutions because Parse accepts only literal argv.
func Evaluate(command string, rules []Rule) Evaluation {
	parsed, err := Parse(command)
	if err != nil {
		return Evaluation{Decision: Forbidden, Reason: err.Error()}
	}
	out := Evaluation{Decision: Allow, Segments: parsed.Segments}
	for _, argv := range parsed.Segments {
		d, reason := builtinDecision(argv)
		matched := d != ""
		if matched {
			out.MatchedRules = append(out.MatchedRules, "builtin:"+argv[0])
		}
		for _, r := range append(DefaultRules(), rules...) {
			if !prefix(argv, r.Pattern) {
				continue
			}
			if !matched || strictest(d, r.Decision) != d {
				d, reason = r.Decision, r.Reason
			}
			matched = true
			out.MatchedRules = append(out.MatchedRules, r.Name)
		}
		if !matched {
			d, reason = Prompt, "命令未命中允许规则，需要逐次审批"
		}
		if strictest(out.Decision, d) != out.Decision || (out.Reason == "" && reason != "") {
			out.Reason = reason
		}
		out.Decision = strictest(out.Decision, d)
	}
	return out
}

func prefix(argv, pattern []string) bool {
	if len(argv) < len(pattern) {
		return false
	}
	for i, v := range pattern {
		if argv[i] != v {
			return false
		}
	}
	return true
}

func builtinDecision(a []string) (Decision, string) {
	cmd := a[0]
	if cmd == "jq" && len(a) == 2 && (a[1] == "--version" || a[1] == "--help") {
		return Allow, "本地工具版本与帮助探测"
	}
	for _, v := range []string{"rm", "sudo", "su", "doas", "chmod", "chown", "mkfs", "dd", "shutdown", "reboot", "env", "printenv", "sh", "bash", "zsh", "fish", "dash", "busybox", "pip", "pip3", "apt", "apt-get", "brew", "npm",
		// Wrappers run their arguments as another command, hiding it from the
		// policy, which only ever sees the wrapper's own name.
		"eval", "exec", "command", "builtin", "source", "xargs", "nohup", "timeout", "nice", "ionice", "setsid", "stdbuf", "script", "watch", "chroot", "unshare", "nsenter"} {
		if cmd == v {
			return Forbidden, "禁止破坏性命令、凭据枚举、shell 包装和软件安装"
		}
	}
	if cmd == "kubectl" {
		mutation := false
		readResources := false
		for _, v := range a[1:] {
			if v == "get" || v == "describe" {
				readResources = true
			}
		}
		for _, v := range a[1:] {
			// Safety restrictions are positional-independent: CLI global flags
			// may precede the verb. Ambiguous values fail closed as well.
			if v == "config" {
				return Forbidden, "不允许读取或修改 kubeconfig"
			}
			if readResources {
				// kubectl matches resource and kind names case-insensitively, so
				// "Secret" and "SECRETS" name the same objects as "secrets".
				for _, res := range strings.Split(strings.ToLower(v), ",") {
					res = strings.Split(res, "/")[0]
					if res == "secret" || res == "secrets" || strings.HasPrefix(res, "secret.") || strings.HasPrefix(res, "secrets.") {
						return Forbidden, "不允许读取 Kubernetes Secret"
					}
				}
				// A manifest (local, URL or kustomization) names its objects
				// outside argv, where the Secret check above cannot see them.
				if kubectlManifestFlag(v) {
					return Forbidden, "读取资源时不允许通过 -f/-k 引用清单，请直接写资源类型与名称"
				}
				if strings.Contains(v, "template-file") || strings.Contains(v, "jsonpath-file") {
					return Forbidden, "不允许从文件加载输出模板"
				}
			}
			if v == "delete" {
				return Forbidden, "删除资源必须使用结构化变更工具"
			}
			for _, verb := range []string{"apply", "patch", "scale", "rollout", "create", "edit", "exec", "cp", "replace", "label", "annotate", "cordon", "uncordon", "drain", "taint", "attach", "run", "set", "autoscale"} {
				if v == verb {
					mutation = true
				}
			}
			for _, flag := range []string{"--token", "--password", "--client-key", "--client-certificate", "--certificate-authority", "--kubeconfig", "--context", "--server", "--cluster", "--user", "--username", "--as", "--as-group", "--as-uid", "--raw", "--tls-server-name", "--insecure-skip-tls-verify", "-s"} {
				if v == flag || strings.HasPrefix(v, flag+"=") {
					return Forbidden, "不允许覆盖运行时配置的身份或集群：" + flag
				}
			}
			if strings.HasPrefix(v, "-s") && !strings.HasPrefix(v, "--") {
				return Forbidden, "不允许覆盖运行时配置的集群"
			}
		}
		if mutation {
			return Prompt, "变更或远程执行需要逐次审批"
		}
		// Allow-listed, not block-listed: debug, port-forward, proxy, plugins
		// and anything kubectl adds later need a person, even under a broad
		// administrator rule — the builtin decision is the stricter one.
		if !kubectlReadOnly(a) {
			return Prompt, "只读范围之外的 kubectl 子命令需要逐次审批"
		}
	}
	if cmd == "tccli" {
		for _, v := range a[1:] {
			if strings.HasPrefix(v, "Delete") || strings.HasPrefix(v, "Terminate") {
				return Forbidden, "破坏性云操作必须使用结构化变更工具"
			}
			for _, flag := range []string{"--secretId", "--secretKey", "--token", "--SecretId", "--SecretKey", "--Token", "--endpoint", "--Endpoint", "--profile", "--root-domain", "--https-proxy", "--use-cvm-role", "--role-arn", "--role-session-name"} {
				if v == flag || strings.HasPrefix(v, flag+"=") {
					return Forbidden, "不允许覆盖运行时注入的云身份或端点"
				}
			}
		}
		if tccliHelp(a) {
			return Allow, "CLI 帮助自发现"
		}
		if len(a) < 3 {
			return "", ""
		}
		product, action := a[1], a[2]
		if !identifier.MatchString(product) || !identifier.MatchString(action) {
			return Prompt, "无法识别 tccli 的产品与接口位置，需要逐次审批"
		}
		if (product == "cls" && action == "SearchLog") || (product == "sts" && action == "GetCallerIdentity") {
			return Allow, "只读云查询；资源权限由配置的凭据限制"
		}
		if strings.HasPrefix(action, "Describe") || strings.HasPrefix(action, "List") {
			// Read-only by Tencent Cloud's naming, but some reads hand back
			// something usable as access: a certificate's private key, a
			// cluster kubeconfig, a VNC login URL, secret or key material.
			for _, word := range sensitiveReads {
				if strings.Contains(strings.ToLower(action), word) {
					return Prompt, "该查询会返回凭据或访问入口，需要逐次审批"
				}
			}
			return Allow, "只读云查询（Describe/List）；资源权限由配置的凭据限制"
		}
		return Prompt, "云操作不在默认只读范围内，需要逐次审批"
	}
	return "", ""
}

// kubectlReadOnly reports whether argv's subcommand is one of kubectl's
// read-only ones. The subcommand is the first token that is neither a global
// flag nor such a flag's value; an unknown flag written without "=" may or may
// not take the next token, so it fails closed rather than guessing.
func kubectlReadOnly(a []string) bool {
	valueFlags := map[string]bool{"-n": true, "--namespace": true, "--request-timeout": true, "--cache-dir": true,
		"-v": true, "--v": true, "--log-file": true, "--log-dir": true, "--vmodule": true, "--profile": true, "--profile-output": true}
	boolFlags := map[string]bool{"--match-server-version": true, "--disable-compression": true, "--warnings-as-errors": true}
	for i := 1; i < len(a); i++ {
		v := a[i]
		switch {
		case !strings.HasPrefix(v, "-"):
			next := ""
			if i+1 < len(a) {
				next = a[i+1]
			}
			switch v {
			case "get", "describe", "logs", "top", "version", "api-resources", "api-versions", "help", "explain", "events":
				return true
			case "cluster-info":
				return next != "dump" // dump pulls every namespace's pods and logs
			case "auth":
				return next == "can-i" || next == "whoami"
			}
			return false
		case strings.Contains(v, "=") || boolFlags[v]:
		case valueFlags[v]:
			i++
		case strings.HasPrefix(v, "-n") && !strings.HasPrefix(v, "--"):
			// -nkube-system: the shorthand with its value attached.
		default:
			return false
		}
	}
	return false
}

// kubectlManifestFlag reports whether v selects objects through a manifest:
// --filename/--kustomize, or -f/-k anywhere in a shorthand group such as
// "-Af" or "-fhttps://…". A group ends at the first shorthand that takes a
// value, because the rest of the token is that value ("-ojsonpath={.kind}").
func kubectlManifestFlag(v string) bool {
	for _, flag := range []string{"--filename", "--kustomize"} {
		if v == flag || strings.HasPrefix(v, flag+"=") {
			return true
		}
	}
	if len(v) < 2 || v[0] != '-' || v[1] == '-' {
		return false
	}
	for _, c := range v[1:] {
		if c == 'f' || c == 'k' {
			return true
		}
		if strings.ContainsRune("olnLs", c) {
			return false
		}
	}
	return false
}

// sensitiveReads are lower-case fragments of Describe/List action names whose
// result is a credential or a way in rather than a description.
var sensitiveReads = []string{"secret", "password", "passwd", "credential", "token", "vnc", "loginkey", "privatekey", "certificate", "kubeconfig", "accesskey"}

// Help occupies a command slot, never an arbitrary argument value. Only the
// documented detail modifier is accepted; identity/endpoint bans run first.
func tccliHelp(a []string) bool {
	for _, pos := range []int{1, 2, 3} {
		if len(a) <= pos || (a[pos] != "help" && a[pos] != "--help") {
			continue
		}
		for _, v := range a[1:pos] {
			if !identifier.MatchString(v) {
				return false
			}
		}
		return len(a) == pos+1 || (len(a) == pos+2 && a[pos+1] == "--detail")
	}
	return false
}

func mergeEnv(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
