// Command jelly is the jelly-agent CLI. Phase 2 first batch establishes the
// cobra command tree (agent / config / tool) on top of the config layer and
// model registry, while preserving the single-turn `jelly agent run --once`
// behavior from Phase 1.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jelly-agent/jelly-agent/internal/config"
	jellymodel "github.com/jelly-agent/jelly-agent/internal/model"
)

// version is the CLI version, overridable at build time via -ldflags.
var version = "0.2.0-dev"

// configPath is the persistent --config flag value.
var configPath string

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "jelly",
		Short:         "jelly-agent —— 基于 ADK-Go 的多模型 Agent 平台",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.PersistentFlags().StringVar(&configPath, "config", "", "配置文件路径（默认搜索 configs/config.yaml、~/.jelly-agent/config.yaml，或回落到 LLM_* 环境变量）")

	root.AddCommand(newAgentCmd(), newConfigCmd(), newToolCmd(), newSessionCmd())
	return root
}

// loadConfig resolves and loads configuration for a command.
func loadConfig() (*config.Config, error) {
	return config.LoadOrEnv(configPath)
}

// loadRegistry loads config and wraps it in a model registry.
func loadRegistry() (*jellymodel.Registry, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return jellymodel.NewRegistry(cfg), nil
}
