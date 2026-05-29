package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "查看与管理 Provider 配置",
	}
	cmd.AddCommand(newConfigListCmd())
	return cmd
}

func newConfigListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出所有 Provider（API Key 脱敏）",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			src := cfg.SourcePath
			if src == "" {
				src = "(未找到配置，且无 LLM_* 环境变量)"
			}
			fmt.Printf("来源: %s   默认: %s\n\n", src, orNone(cfg.DefaultProvider))

			if len(cfg.Providers) == 0 {
				fmt.Println("(无 Provider)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tMODEL\tBASE_URL\tAPI_KEY")
			for _, p := range cfg.Providers {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, p.Model, p.BaseURL, config.MaskKey(p.APIKey))
			}
			return w.Flush()
		},
	}
}

func orNone(s string) string {
	if s == "" {
		return "(未设置)"
	}
	return s
}
