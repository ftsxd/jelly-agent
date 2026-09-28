// Package execution implements the opt-in generic diagnostic executor.
package execution

import (
	"fmt"
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
	// KubeconfigEnv names a server variable containing an inline read-only
	// kubeconfig. It is materialized privately, never a host path mount.
	KubeconfigEnv string `json:"kubeconfig_env,omitempty" yaml:"kubeconfig_env,omitempty"`
	Network       bool   `json:"network" yaml:"network,omitempty"`
	Rules         []Rule `json:"rules,omitempty" yaml:"rules,omitempty"`
	// Writes stay opt-in. Elevated sources are injected only after one-use approval.
	WriteApproval      bool              `json:"write_approval,omitempty" yaml:"write_approval,omitempty"`
	WriteEnv           map[string]string `json:"write_env,omitempty" yaml:"write_env,omitempty"`
	WriteKubeconfigEnv string            `json:"write_kubeconfig_env,omitempty" yaml:"write_kubeconfig_env,omitempty"`
}

type Config struct {
	Enabled     bool      `json:"enabled" yaml:"enabled,omitempty"`
	Backend     string    `json:"backend" yaml:"backend,omitempty"`
	Image       string    `json:"image,omitempty" yaml:"image,omitempty"`
	TimeoutSec  int       `json:"timeout_sec" yaml:"timeout_sec,omitempty"`
	MaxOutputKB int       `json:"max_output_kb" yaml:"max_output_kb,omitempty"`
	Profiles    []Profile `json:"profiles" yaml:"profiles,omitempty"`
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
		for _, variables := range []map[string]string{p.Env, p.WriteEnv} {
			for k, source := range variables {
				if !envName.MatchString(k) || !envName.MatchString(source) || dangerousEnv(k) {
					return fmt.Errorf("环境变量映射无效或会改变执行环境：%s", k)
				}
			}
		}
		if (p.KubeconfigEnv != "" && !envName.MatchString(p.KubeconfigEnv)) || (p.WriteKubeconfigEnv != "" && !envName.MatchString(p.WriteKubeconfigEnv)) {
			return fmt.Errorf("kubeconfig 必须引用服务端环境变量名称")
		}
		if !p.WriteApproval && (len(p.WriteEnv) > 0 || p.WriteKubeconfigEnv != "") {
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
	return nil
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
	for _, v := range []string{"rm", "sudo", "su", "doas", "chmod", "chown", "mkfs", "dd", "shutdown", "reboot", "env", "printenv", "sh", "bash", "zsh", "fish", "dash", "busybox", "pip", "pip3", "apt", "apt-get", "brew", "npm"} {
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
				for _, res := range strings.Split(v, ",") {
					res = strings.Split(res, "/")[0]
					if res == "secret" || res == "secrets" || strings.HasPrefix(res, "secret.") || strings.HasPrefix(res, "secrets.") {
						return Forbidden, "不允许读取 Kubernetes Secret"
					}
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
			for _, flag := range []string{"--token", "--password", "--client-key", "--client-certificate", "--certificate-authority", "--kubeconfig", "--context", "--server", "--cluster", "--user", "--username", "--as", "--as-group", "--raw", "--tls-server-name", "--insecure-skip-tls-verify", "-s"} {
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
		if len(a) < 3 {
			return "", ""
		}
		action := a[2]
		if (len(a) == 3 && action == "help") || (len(a) == 4 && (a[3] == "help" || a[3] == "--help")) {
			return Allow, "CLI 帮助自发现"
		}
		if a[1] == "cls" && (strings.HasPrefix(action, "Describe") || strings.HasPrefix(action, "List") || action == "SearchLog") {
			return Allow, "只读云查询；资源权限由配置的只读凭据限制"
		}
		if strings.HasPrefix(action, "Describe") || strings.HasPrefix(action, "List") {
			return "", ""
		}
		return Prompt, "云操作不在默认只读范围内，需要逐次审批"
	}
	return "", ""
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
