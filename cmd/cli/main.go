// Command jelly is the jelly-agent CLI. Phase 2 first batch establishes the
// cobra command tree (agent / config / tool) on top of the config layer and
// model registry, while preserving the single-turn `jelly agent run --once`
// behavior from Phase 1.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"
	adktool "google.golang.org/adk/tool"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/logging"
	"github.com/jelly-agent/jelly-agent/internal/memory"
	"github.com/jelly-agent/jelly-agent/internal/telemetry"
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

// setupLogging installs the structured logger from config.
//
// It runs before anything else a command does, so a failure later in startup is
// reported in the same format as everything after it.
func setupLogging(cfg *config.Config) {
	logging.Setup(logging.Config{
		Level:     cfg.Logging.Level,
		Format:    cfg.Logging.Format,
		AddSource: cfg.Logging.AddSource,
		Service:   "jelly-agent",
		Version:   version,
	})
}

// startTracing installs the span exporter for a command that runs an agent, and
// returns the flush to defer.
//
// The flush is not optional bookkeeping. Spans are batched, so without it the
// last run's spans — the ones you started the command to look at — are dropped
// when the process exits.
//
// An unreachable collector is reported and then ignored: tracing is an aid, and
// a diagnosis must not fail because a trace backend is down.
func startTracing(cfg *config.Config) func() {
	t := cfg.Tracing
	ratio := 1.0
	if t.SampleRatio != nil {
		ratio = *t.SampleRatio
	}
	shutdown, err := telemetry.Start(context.Background(), telemetry.Config{
		Enabled:        t.Enabled,
		Endpoint:       t.Endpoint,
		Protocol:       t.Protocol,
		ServiceName:    t.Service,
		Version:        version,
		SampleRatio:    ratio,
		Insecure:       t.Insecure,
		CaptureContent: t.CaptureContent,
	})
	if err != nil {
		slog.Warn("链路追踪未启用", logging.Err(err))
		return func() {}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			slog.Warn("链路追踪 flush 失败", logging.Err(err))
		}
	}
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
