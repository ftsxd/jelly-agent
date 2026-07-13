// Command jelly is the jelly-agent CLI. Phase 2 first batch establishes the
// cobra command tree (agent / config / tool) on top of the config layer and
// model registry, while preserving the single-turn `jelly agent run --once`
// behavior from Phase 1.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	adktool "google.golang.org/adk/tool"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/memory"
)

// appName and userID are sourced from the engine so the CLI and web server
// share one identity for sessions and memory.
const (
	appName = engine.AppName
	userID  = engine.UserID
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

	root.AddCommand(newAgentCmd(), newConfigCmd(), newToolCmd(), newSessionCmd(), newServeCmd(), newAdminCmd())
	return root
}

// loadConfig resolves and loads configuration for a command.
func loadConfig() (*config.Config, error) {
	return config.LoadOrEnv(configPath)
}

// loadEngine loads config and wraps it in the shared runtime engine.
func loadEngine() (*engine.Engine, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return engine.New(cfg), nil
}

// loadMemory builds the L1 core-memory store from the config's memory section,
// falling back to defaults when the section is absent.
func loadMemory() (*memory.Core, error) {
	eng, err := loadEngine()
	if err != nil {
		return nil, err
	}
	return eng.Core()
}

// listBuiltins builds the tool set for display (jelly tool list, /tools): L1
// core tools always, plus load_memory when L2 search is enabled. It does not
// open the search index.
func listBuiltins() ([]adktool.Tool, error) {
	eng, err := loadEngine()
	if err != nil {
		return nil, err
	}
	core, err := eng.Core()
	if err != nil {
		return nil, err
	}
	return eng.Tools(core, eng.SearchEnabled())
}
