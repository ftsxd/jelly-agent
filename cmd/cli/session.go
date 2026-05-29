package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	adksession "google.golang.org/adk/session"

	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
)

func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "查看历史会话",
	}
	cmd.AddCommand(newSessionListCmd())
	return cmd
}

func newSessionListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出持久化的历史会话",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := jellysession.NewSQLite("")
			if err != nil {
				return err
			}
			resp, err := svc.List(cmd.Context(), &adksession.ListRequest{AppName: appName, UserID: userID})
			if err != nil {
				return fmt.Errorf("list sessions: %w", err)
			}
			if len(resp.Sessions) == 0 {
				fmt.Println("(无历史会话)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "SESSION ID\tEVENTS")
			for _, s := range resp.Sessions {
				fmt.Fprintf(w, "%s\t%d\n", s.ID(), s.Events().Len())
			}
			return w.Flush()
		},
	}
}
