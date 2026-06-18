// Command jelly-server runs the jelly-agent web dashboard: the same runtime as
// the CLI, exposed over a small REST + SSE API with the embedded Vue frontend.
// It listens on :6185 by default (PLAN §6.1) and serves a single binary.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/engine"
	"github.com/jelly-agent/jelly-agent/internal/server"
	"github.com/jelly-agent/jelly-agent/web"
)

func main() {
	addr := envOr("JELLY_ADDR", ":6185")
	configPath := os.Getenv("JELLY_CONFIG")

	cfg, err := config.LoadOrEnv(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}

	eng := engine.New(cfg)
	srv := server.New(eng, web.DistFS()).WithConfigPath(configPath)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: chat responses are long-lived SSE streams.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go srv.Watch(ctx)  // hot-reload on external config file edits
	srv.StartBots(ctx) // launch enabled messaging-platform bots (DingTalk, …)

	go func() {
		embedded := "已嵌入前端"
		if web.DistFS() == nil {
			embedded = "前端未构建（仅 API，运行 `npm run build` 后重新编译）"
		}
		fmt.Printf("jelly-agent dashboard 监听 http://localhost%s  [%s]\n", addr, embedded)
		fmt.Printf("配置来源: %s   默认 Provider: %s\n", cfg.SourcePath, cfg.DefaultProvider)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "error: server: %v\n", err)
			stop()
		}
	}()

	<-ctx.Done()
	fmt.Println("\n正在关闭服务器…")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		fmt.Fprintf(os.Stderr, "error: shutdown: %v\n", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
