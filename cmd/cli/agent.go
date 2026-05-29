package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "管理与运行 Agent",
	}
	cmd.AddCommand(newAgentRunCmd(), newAgentListCmd())
	return cmd
}

func newAgentRunCmd() *cobra.Command {
	var (
		provider string
		once     string
	)
	cmd := &cobra.Command{
		Use:   "run [agent]",
		Short: "运行 Agent（--once 单次问答；交互式多轮见第二批）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := loadRegistry()
			if err != nil {
				return err
			}
			a, prov, err := buildAgent(reg, provider)
			if err != nil {
				return err
			}

			question := strings.TrimSpace(once)
			if question == "" {
				return fmt.Errorf("交互式多轮对话将在 Phase 2 第二批提供；当前请用 --once \"问题\" 进行单次问答")
			}
			sessionID := fmt.Sprintf("cli-%d", time.Now().UnixNano())
			return runOnce(cmd.Context(), a, prov, sessionID, question)
		},
	}
	cmd.Flags().StringVarP(&provider, "provider", "p", "", "使用的 Provider 名称（默认用配置的 default_provider）")
	cmd.Flags().StringVar(&once, "once", "", "单次问答的问题，输出后退出")
	return cmd
}

func newAgentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出已定义的 Agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			// 第一批仅内置 root agent；从 YAML 动态加载 Agent 定义在后续批次。
			fmt.Println("NAME  DESCRIPTION")
			fmt.Println("root  jelly-agent root agent with web search")
			return nil
		},
	}
}
