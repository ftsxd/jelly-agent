package main

import (
	"github.com/spf13/cobra"

	"github.com/jelly-agent/jelly-agent/internal/sandbox"
)

// newSandboxHelperCmd registers the hidden re-entry point the Linux sandbox uses.
//
// Go cannot run code between fork and exec, so there is no way to apply Landlock
// to a child from the parent. The way out — the same one Codex CLI takes — is to
// exec this binary again with this subcommand: it restricts *itself*, then execs
// the real target, which therefore starts already confined and never gets an
// unrestricted instruction.
//
// It is hidden because it is machine-facing: sandbox.Run writes the argv, and a
// human typing it gains nothing. Flag parsing is disabled so the target's own
// flags travel through untouched.
func newSandboxHelperCmd() *cobra.Command {
	return &cobra.Command{
		Use:                sandbox.HelperCommand + " [--ro=PATH]… [--rw=PATH]… [--no-net] -- CMD [ARG…]",
		Short:              "内部使用：给自己套上沙箱限制后 exec 目标命令",
		Hidden:             true,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			// On success this never returns: the process image is replaced.
			return sandbox.Helper(args)
		},
	}
}
