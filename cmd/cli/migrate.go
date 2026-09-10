package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jelly-agent/jelly-agent/internal/migrate"
	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
	"github.com/jelly-agent/jelly-agent/internal/storage"
)

func newMigrateCmd() *cobra.Command {
	var (
		from      string
		dryRun    bool
		allowDrop bool
	)
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "把一个状态库的内容搬到 storage.dsn 指向的库",
		Long: `把 --from 指向的状态库搬到配置里 storage.dsn 指向的库。

典型用法是从 SQLite 搬到 PostgreSQL：

    # 1. 先在目标库上建表
    psql "$DSN" -f migrations/postgres/0001_init.sql

    # 2. 看看会搬多少（不写任何东西）
    jelly migrate --from ~/.jelly-agent/state.db --dry-run

    # 3. 停掉服务，再搬
    jelly migrate --from ~/.jelly-agent/state.db

服务必须先停。这条命令不加锁，边搬边写会漏掉搬运过程中新写入的行 —— 而它
搬完会核对行数，所以那种情况会被报出来，不会静悄悄地少。

可以重跑：每条插入都是 ON CONFLICT DO NOTHING，中途断了直接再来一次。目标库
里已经有的行会被跳过，但会先核对内容 —— 主键相同而内容不同的行会报出来并中止，
因为那说明搬错了库，或者上次搬完之后源库又被写过。

回滚就是把 storage.dsn 改回去，源库自始至终没有被改动。但要注意时间窗口 ——
搬完之后服务在新库上写下的东西，回滚时不会回到源库。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				return fmt.Errorf("--from 是必填的：源状态库的路径或 DSN")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.Storage.DSN == "" {
				return fmt.Errorf("配置里没有 storage.dsn —— 目标就是默认的那个 SQLite 文件，" +
					"没有什么可搬的")
			}
			if cfg.Storage.DSN == from {
				return fmt.Errorf("源和目标是同一个库")
			}

			src, err := storage.Open(from)
			if err != nil {
				return fmt.Errorf("打开源库: %w", err)
			}
			defer src.Close()

			// ADK's four tables are created by its own AutoMigrate, not by
			// the migration file, so the target needs the service opened once
			// before anything can be copied into them.
			//
			// Not on a dry run. Opening it runs AutoMigrate, which creates
			// those tables — a command whose entire promise is "writes
			// nothing" was leaving four tables behind on a database somebody
			// ran it against to decide whether to migrate at all.
			if !dryRun {
				_, closeSvc, err := jellysession.New(cfg.Storage.DSN)
				if err != nil {
					return fmt.Errorf("在目标库上准备 ADK 的会话表: %w", err)
				}
				defer closeSvc()
			}
			dst, err := storage.Open(cfg.Storage.DSN)
			if err != nil {
				return fmt.Errorf("打开目标库: %w", err)
			}
			defer dst.Close()

			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "表\t复制\t已存在")
			rep, err := migrate.Run(cmd.Context(), src, dst, migrate.Options{
				DryRun:               dryRun,
				AllowDroppingColumns: allowDrop,
				Notes:                func(msg string) { fmt.Fprintln(os.Stderr, "  "+msg) },
				Progress: func(table string, copied, skipped int64) {
					fmt.Fprintf(w, "%s\t%d\t%d\n", table, copied, skipped)
					w.Flush()
				},
			})
			if err != nil {
				return err
			}
			w.Flush()
			fmt.Println()
			fmt.Println(rep.Note)

			if dryRun {
				fmt.Println("\n（--dry-run：什么都没有写）")
				return nil
			}

			// Checked rather than assumed. The failure this catches is the one
			// that actually happens: a copy run while the service was still
			// writing, or against the wrong database.
			bad, err := migrate.Verify(cmd.Context(), src, dst)
			if err != nil {
				return fmt.Errorf("核对行数: %w", err)
			}
			if len(bad) > 0 {
				for _, m := range bad {
					fmt.Fprintf(os.Stderr, "行数对不上 —— %s\n", m)
				}
				return fmt.Errorf("迁移完成但核对没过 —— 服务是不是还在写源库？" +
					"停掉之后可以直接重跑这条命令")
			}
			fmt.Println("行数核对通过。")
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "源状态库（路径或 DSN）")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "只统计，不写入")
	cmd.Flags().BoolVar(&allowDrop, "allow-dropping-columns", false,
		"源库有目标库没有的列时照搬其余的，丢掉那些列的值（默认是拒绝并报出列名）")
	return cmd
}
