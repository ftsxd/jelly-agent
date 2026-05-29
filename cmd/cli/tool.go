package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	jellytool "github.com/jelly-agent/jelly-agent/internal/tool"
)

func newToolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tool",
		Short: "查看内置工具",
	}
	cmd.AddCommand(newToolListCmd())
	return cmd
}

func newToolListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出已注册的工具",
		RunE: func(cmd *cobra.Command, args []string) error {
			tools, err := jellytool.Builtins()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tDESCRIPTION")
			for _, t := range tools {
				fmt.Fprintf(w, "%s\t%s\n", t.Name(), t.Description())
			}
			return w.Flush()
		},
	}
}
