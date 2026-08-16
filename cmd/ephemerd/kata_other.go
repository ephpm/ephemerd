//go:build !linux

package main

import (
	"fmt"
	"log/slog"

	"github.com/ephpm/ephemerd/pkg/config"
)

// checkKataPrereqs rejects the Kata runtime on non-Linux hosts.
//
// [runner.linux] runtime only governs Linux job containers, and Kata is a
// Linux-only runtime. A Windows or macOS host runs its Linux jobs inside
// the Linux VM (see [vm.linux]), whose in-VM ephemerd has its own config
// — so setting this key on the host config would have no effect on the
// containers it names. Rejecting it beats silently ignoring it.
func checkKataPrereqs(cfg *config.Config, _ *slog.Logger) error {
	if cfg.Runner.Linux.ResolvedRuntime() != config.LinuxRuntimeKata {
		return nil
	}
	return fmt.Errorf(`runner.linux.runtime = "kata" is only supported on Linux hosts ` +
		`(Linux jobs on this host run inside the [vm.linux] guest; set the key in that ` +
		`VM's own config instead)`)
}
