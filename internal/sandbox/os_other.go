//go:build !linux && !darwin

package sandbox

import (
	"context"
	"errors"
)

// Neither Seatbelt nor Landlock exists here, so the "os" backend is simply
// absent: selectBackend degrades to native and says so.

// systemReadPaths has no meaning without a confinement layer to feed it, but it
// keeps the shared argv builder compiling on every platform.
var systemReadPaths []string

func osAvailable() bool { return false }

func osUnavailable() string {
	return "本平台没有系统级沙箱，os 后端只支持 macOS（Seatbelt）与 Linux（Landlock）"
}

func osDetail() string { return osUnavailable() }

func runOS(ctx context.Context, p Policy, s Spec) (Result, error) {
	return runNative(ctx, p, s)
}

// HelperCommand and Helper exist on every platform so the CLI can register the
// subcommand unconditionally; only Linux has anything to do in it.
const HelperCommand = "__sandbox"

func Helper([]string) error {
	return errors.New("sandbox helper: 只有 Linux 需要这个子命令")
}
