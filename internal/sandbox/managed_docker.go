package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	LabelScope = "io.jelly-agent.scope"
	LabelExec  = "io.jelly-agent.exec"
	LabelOwner = "io.jelly-agent.owner"
)

// Identity is persisted before create; container ID is committed before start.
type ContainerIdentity struct {
	Name, ID, ExecID, Owner, Scope, Daemon string
}
type ManagedContainer struct {
	Identity ContainerIdentity
	Created  func(string) error
	Starting func() error
}
type ContainerInfo struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels"`
	Running bool              `json:"running"`
	Started string            `json:"started"`
	Exit    int               `json:"exit"`
}

var dockerID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var execIdentity = regexp.MustCompile(`^exec_[a-f0-9]{32}$`)
var ownerIdentity = regexp.MustCompile(`^[a-f0-9]{32}$`)

// DockerDaemonID binds recovery to the same daemon, not whichever context
// happens to be selected later. No full info/inspect payload is recorded.
func DockerDaemonID(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ID}}").Output()
	id := strings.TrimSpace(string(b))
	if err != nil || id == "" || len(id) > 256 {
		return "", fmt.Errorf("Docker 服务身份不可验证")
	}
	return id, nil
}

func checkContainerIdentity(ctx context.Context, id ContainerIdentity) error {
	if !execIdentity.MatchString(id.ExecID) || id.Name != "jelly-"+id.ExecID || !ownerIdentity.MatchString(id.Owner) || !ownerIdentity.MatchString(id.Scope) || (id.ID != "" && !dockerID.MatchString(id.ID)) || id.Daemon == "" {
		return fmt.Errorf("执行容器身份无效")
	}
	current, err := DockerDaemonID(ctx)
	if err != nil {
		return err
	}
	if current != id.Daemon {
		return fmt.Errorf("Docker 服务身份已改变，保留资源等待原执行环境恢复")
	}
	return nil
}

// InspectManaged exposes only identity/state, never Env or kubeconfig content.
func InspectManaged(ctx context.Context, id ContainerIdentity) (ContainerInfo, bool, error) {
	if err := checkContainerIdentity(ctx, id); err != nil {
		return ContainerInfo{}, false, err
	}
	target := id.ID
	if target == "" {
		target = id.Name
	}
	format := `{"id":{{json .Id}},"name":{{json .Name}},"labels":{{json .Config.Labels}},"running":{{json .State.Running}},"started":{{json .State.StartedAt}},"exit":{{json .State.ExitCode}}}`
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", "--format", format, target)
	res, err := capture(cmd, 16<<10)
	if err != nil {
		return ContainerInfo{}, false, fmt.Errorf("无法核验执行容器")
	}
	if res.ExitCode != 0 {
		if strings.Contains(res.Stderr, "No such object:") || strings.Contains(res.Stderr, "No such container:") {
			return ContainerInfo{}, true, nil
		}
		return ContainerInfo{}, false, fmt.Errorf("无法核验执行容器，保留恢复记录")
	}
	var info ContainerInfo
	if res.Truncated || json.Unmarshal([]byte(res.Stdout), &info) != nil || !dockerID.MatchString(info.ID) || strings.TrimPrefix(info.Name, "/") != id.Name || (id.ID != "" && info.ID != id.ID) || info.Labels[LabelExec] != id.ExecID || info.Labels[LabelOwner] != id.Owner || info.Labels[LabelScope] != id.Scope {
		return ContainerInfo{}, false, fmt.Errorf("容器所属标记不匹配，拒绝回收")
	}
	return info, false, nil
}

func RemoveManaged(ctx context.Context, id ContainerIdentity) error {
	info, absent, err := InspectManaged(ctx, id)
	if err != nil || absent {
		return err
	}
	res, err := capture(exec.CommandContext(ctx, "docker", "rm", "-f", info.ID), 4<<10)
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("执行容器回收失败，将保留恢复记录")
	}
	id.ID = info.ID
	_, absent, err = InspectManaged(ctx, id)
	if err != nil {
		return err
	}
	if !absent {
		return fmt.Errorf("执行容器仍存在，将继续回收")
	}
	return nil
}

// DiscoverManaged catches create RPCs that finish after a crashed client.
// Labels alone never authorize deletion: the journal and InspectManaged do.
func DiscoverManaged(ctx context.Context, scope, daemon string) ([]ContainerInfo, error) {
	if !ownerIdentity.MatchString(scope) {
		return nil, fmt.Errorf("执行资源范围无效")
	}
	actual, err := DockerDaemonID(ctx)
	if err != nil {
		return nil, err
	}
	if actual != daemon {
		return nil, fmt.Errorf("Docker 服务身份已改变")
	}
	res, err := capture(exec.CommandContext(ctx, "docker", "ps", "-a", "--no-trunc", "--filter", "label="+LabelScope+"="+scope, "--format", "{{.ID}}"), 1<<20)
	if err != nil || res.ExitCode != 0 || res.Truncated {
		return nil, fmt.Errorf("无法发现遗留执行容器")
	}
	var out []ContainerInfo
	for _, id := range strings.Fields(res.Stdout) {
		if !dockerID.MatchString(id) {
			return nil, fmt.Errorf("遗留容器身份无效")
		}
		// Read just labels; the caller must match the full journal authority.
		part, err := capture(exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .Config.Labels}}", id), 16<<10)
		if err != nil || part.ExitCode != 0 || part.Truncated {
			continue
		}
		var labels map[string]string
		if json.Unmarshal([]byte(part.Stdout), &labels) == nil && labels[LabelScope] == scope {
			out = append(out, ContainerInfo{ID: id, Labels: labels})
		}
	}
	return out, nil
}

func runManagedDocker(ctx context.Context, p Policy, s Spec) (res Result, err error) {
	m := s.Managed
	id := m.Identity
	if err = checkContainerIdentity(ctx, id); err != nil {
		return Result{ExitCode: -1}, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	args := dockerArgs(p, s)
	// Retain exited containers until the outcome is collected and journaled.
	create := []string{"create", "--name", id.Name, "--label", LabelScope + "=" + id.Scope, "--label", LabelExec + "=" + id.ExecID, "--label", LabelOwner + "=" + id.Owner}
	create = append(create, args[2:]...) // Only drop dockerArgs' own --rm, never a business argument.
	cmd := exec.CommandContext(ctx, "docker", create...)
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	created, createErr := capture(cmd, 4<<10)
	container := strings.TrimSpace(created.Stdout)
	if createErr != nil || created.ExitCode != 0 || !dockerID.MatchString(container) {
		return Result{ExitCode: -1}, fmt.Errorf("容器创建未确认，恢复器将核验遗留资源")
	}
	id.ID = container
	if m.Created == nil || m.Starting == nil {
		return Result{ExitCode: -1}, fmt.Errorf("持久化生命周期回调缺失，容器未启动")
	}
	if err = m.Created(container); err != nil {
		return Result{ExitCode: -1}, err
	}
	if err = checkContainerIdentity(ctx, id); err != nil {
		return Result{ExitCode: -1}, err
	}
	if err = m.Starting(); err != nil {
		return Result{ExitCode: -1}, err
	}
	cmd = exec.CommandContext(ctx, "docker", "start", "-a", "-i", container)
	cmd.WaitDelay = 2 * time.Second
	res, err = capture(cmd, p.MaxOutput)
	res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	res.Cancelled = errors.Is(ctx.Err(), context.Canceled)
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	check, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	info, absent, inspectErr := InspectManaged(check, id)
	if inspectErr != nil || absent || info.Running {
		return res, fmt.Errorf("容器执行结果未确认，禁止自动重试")
	}
	res.Started = info.Started != "" && !strings.HasPrefix(info.Started, "0001-")
	if !res.Started {
		if res.ExitCode == 0 {
			res.ExitCode = -1
		}
		return res, fmt.Errorf("容器命令未启动，不能把未运行容器的默认退出码当成成功")
	}
	res.ExitCode = info.Exit
	return res, err
}
