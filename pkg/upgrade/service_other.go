//go:build !windows

package upgrade

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
)

// RestartService is Windows-only. systemd and launchd both accept a restart
// request from a process inside the unit — `systemctl restart --no-block`
// hands the job to PID 1, `launchctl kickstart -k` to launchd — so the Unix
// paths need no detached helper and never call this.
func RestartService(_ context.Context, name string, _ RestartOptions, _ *slog.Logger) error {
	return fmt.Errorf("restarting service %s via the service control manager is only supported on Windows (this is %s); use %q", name, runtime.GOOS, manualRestartHint)
}
