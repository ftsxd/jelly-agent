package tool

import (
	"github.com/jelly-agent/jelly-agent/internal/execution"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

const ShellExecName = "shell_exec"

// The agent identity is captured when building each node, never from model args.
func NewShellExecTool(runtime execution.Runtime, agent string) (adktool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        ShellExecName,
		Description: "在完整沙箱中执行诊断命令，启用审批的配置可申请逐次写操作。没有专用工具时使用，必须说明目的并选择已分配的执行配置。\n" + runtime.Config.Instruction(agent),
	}, func(tc adktool.Context, req execution.Request) (execution.Observation, error) {
		if confirmation := tc.ToolConfirmation(); confirmation != nil {
			execution.ApprovalStarted(tc, tc.InvocationID())
			if !confirmation.Confirmed {
				return execution.Observation{Evaluation: execution.Evaluation{Decision: execution.Forbidden, Reason: "用户已拒绝此命令"}, Profile: req.Profile, ExitCode: -1, Error: "用户已拒绝此命令，未执行"}, nil
			}
			return runtime.ExecuteApproved(tc, agent, tc.SessionID(), tc.FunctionCallID(), req), nil
		}
		check := runtime.Check(agent, req)
		// A session switched to "ask for every command" stops the direct
		// reads and the session grants alike, until it is switched back.
		strict := runtime.Approvals != nil && check.Decision != execution.Forbidden && runtime.Approvals.Strict(tc, tc.SessionID())
		if strict && check.Decision == execution.Allow {
			check.Decision, check.Reason = execution.Prompt, execution.StrictReason
		}
		if strict && !runtime.ApprovalEnabled(agent, req.Profile) {
			return execution.Observation{Evaluation: execution.Evaluation{Decision: execution.Forbidden, Reason: "本会话已设为逐条审批，但该执行配置不能申请审批"}, Profile: req.Profile, ExitCode: -1, Error: "本会话已设为逐条审批，但该执行配置不能申请审批，命令未执行"}, nil
		}
		// A command the environment lacks is refused by Execute before it would
		// run, so asking a person to approve it would only add a dead card.
		if check.Decision == execution.Prompt && runtime.ApprovalEnabled(agent, req.Profile) && runtime.Approvals != nil && runtime.MissingCommand(agent, req) == "" {
			// A person already said "don't ask again" for this class in this
			// session: run it as approved instead of raising another card.
			if g, ok := runtime.Approvals.FindGrant(tc, runtime.Config, agent, tc.SessionID(), req); ok && !strict {
				return runtime.ExecuteGranted(tc, agent, tc.SessionID(), req, g), nil
			}
			a, err := runtime.Approvals.Create(tc, runtime.Config, agent, tc.SessionID(), tc.InvocationID(), tc.FunctionCallID(), req)
			if err != nil {
				return execution.Observation{}, err
			}
			if err := tc.RequestConfirmation(check.Reason, map[string]any{"approval_id": a.ID}); err != nil {
				return execution.Observation{}, err
			}
			return execution.Observation{Evaluation: check, Profile: req.Profile, ApprovalRequired: true, ApprovalID: a.ID, ExitCode: -1}, nil
		}
		return runtime.Execute(tc, agent, tc.SessionID(), req), nil
	})
}
