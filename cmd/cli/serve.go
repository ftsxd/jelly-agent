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

	"github.com/spf13/cobra"

	"github.com/jelly-agent/jelly-agent/internal/server"
	"github.com/jelly-agent/jelly-agent/web"
)

func newServeCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "启动 Web 控制台（REST + SSE + 内嵌前端）",
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := loadEngine()
			if err != nil {
				return err
			}
			srv := server.New(eng, web.DistFS()).WithConfigPath(configPath)
			httpSrv := &http.Server{
				Addr:              addr,
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				// No write timeout: chat is a long-lived SSE stream.
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			go srv.Watch(ctx) // hot-reload on external config file edits

			errc := make(chan error, 1)
			go func() {
				front := "已嵌入前端"
				if web.DistFS() == nil {
					front = "前端未构建（仅 API）"
				}
				fmt.Printf("jelly-agent 控制台 → http://localhost%s  [%s]\n", addr, front)
				fmt.Printf("配置来源: %s   默认 Provider: %s\n", eng.Config().SourcePath, eng.Config().DefaultProvider)
				if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					errc <- err
				}
			}()

			select {
			case err := <-errc:
				return err
			case <-ctx.Done():
				fmt.Println("\n正在关闭服务器…")
				shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return httpSrv.Shutdown(shutCtx)
			}
		},
	}
	cmd.Flags().StringVar(&addr, "addr", ":6185", "监听地址")
	return cmd
}
