package main

import (
	"fmt"
	"strings"

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
		Short: "运行 Agent（默认进入交互式多轮；--once 单次问答后退出）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := loadEngine()
			if err != nil {
				return err
			}
			setupLogging(eng.Config())
			defer startTracing(eng.Config())()

			a, prov, _, search, err := eng.BuildAgent(provider)
			if err != nil {
				return err
			}

			if question := strings.TrimSpace(once); question != "" {
				return runOnce(cmd.Context(), eng, a, prov, search, newSessionID(), question)
			}
			return runInteractive(cmd.Context(), eng, a, prov, search)
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
