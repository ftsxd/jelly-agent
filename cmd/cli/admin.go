package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "管理 Web 控制台管理员"}
	cmd.AddCommand(&cobra.Command{
		Use:   "set-password <username>",
		Short: "从标准输入读取密码并设置管理员",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			username := strings.TrimSpace(args[0])
			if username == "" {
				return fmt.Errorf("管理员用户名不能为空")
			}
			password, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1025))
			if err != nil {
				return fmt.Errorf("读取密码: %w", err)
			}
			password = []byte(strings.TrimSpace(string(password)))
			if len(password) < 12 {
				return fmt.Errorf("管理员密码至少需要 12 个字符")
			}
			path, err := adminConfigPath()
			if err != nil {
				return err
			}
			cfg, err := config.LoadRaw(path)
			if os.IsNotExist(err) {
				cfg = &config.Config{}
			} else if err != nil {
				return err
			}
			hash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
			if err != nil {
				return fmt.Errorf("生成密码哈希: %w", err)
			}
			cfg.Web.Admin = config.Admin{Username: username, PasswordHash: string(hash)}
			if err := config.Save(cfg, path); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "管理员 %q 已保存到 %s\n", username, path)
			return nil
		},
	})
	return cmd
}

func adminConfigPath() (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	if cfg.SourcePath == "(env)" {
		return "", fmt.Errorf("当前只使用 LLM_* 环境变量；请先创建 configs/config.yaml 或通过 --config 指定配置文件")
	}
	if cfg.SourcePath != "" {
		return cfg.SourcePath, nil
	}
	return config.DefaultUserConfigPath()
}
