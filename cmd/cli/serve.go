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
			if password, err := server.BootstrapAdmin(eng.Config()); err != nil {
				return err
			} else if password != "" {
				fmt.Printf("\n首次管理员账户已创建：用户名 admin\n一次性初始密码：%s\n请立刻登录控制台并修改密码。\n\n", password)
				eng, err = loadEngine()
				if err != nil {
					return err
				}
			}
			if err := server.ValidateAdmin(eng.Config().Web.Admin); err != nil {
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

			go srv.Watch(ctx)  // hot-reload on external config file edits
			srv.StartBots(ctx) // launch enabled messaging-platform bots (DingTalk, …)
			srv.StartSchedules(ctx)

			errc := make(chan error, 1)
			go func() {
				front := "已嵌入前端"
				if web.DistFS() == nil {
					front = "前端未构建（仅 API）"
				}
				fmt.Printf("jelly-agent 控制台 → http://%s  [%s]\n", addr, front)
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
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:6185", "监听地址（默认仅本机；公开访问请显式指定）")
	return cmd
}
