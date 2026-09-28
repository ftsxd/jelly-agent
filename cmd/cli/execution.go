package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// Can run in a separate supervisor process. It only cleans journal-authorised
// resources whose leases expired, and never resumes the original command.
func newExecutionCmd() *cobra.Command {
	var watch bool
	cmd := &cobra.Command{Use: "execution", Short: "执行资源维护"}
	recover := &cobra.Command{Use: "recover", Short: "回收本机已失联的执行容器与凭据目录", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		eng, err := loadEngine()
		if err != nil {
			return err
		}
		defer eng.Close()
		journal, err := eng.ExecutionJournal()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			pass, done := context.WithTimeout(ctx, 25*time.Second)
			n, err := journal.Recover(pass)
			done()
			if err != nil {
				if !watch {
					return err
				}
				fmt.Fprintln(cmd.ErrOrStderr(), "资源回收未完成，保留记录：", err)
			}
			if n > 0 || !watch {
				fmt.Fprintf(cmd.OutOrStdout(), "已回收 %d 条执行资源；结果未知的命令不会重放。\n", n)
			}
			if !watch {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	}}
	recover.Flags().BoolVar(&watch, "watch", false, "持续回收（可由独立服务守护）")
	cmd.AddCommand(recover)
	return cmd
}
