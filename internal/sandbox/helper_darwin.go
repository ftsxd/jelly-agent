package sandbox

import "errors"

// HelperCommand is registered on every platform so the CLI's command tree does
// not change shape per OS, but macOS confines via sandbox-exec in the parent and
// never re-executes itself.
const HelperCommand = "__sandbox"

func Helper([]string) error {
	return errors.New("sandbox helper: macOS 走 sandbox-exec，不需要这个子命令")
}
