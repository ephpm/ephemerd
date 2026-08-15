//go:build linux

package upgrade

import (
	"fmt"
	"os/exec"
	"syscall"
)

// serviceName is the systemd unit installed by `ephemerd install`
// (cmd/ephemerd/install_linux.go).
const serviceName = "ephemerd"

// manualRestartHint is what an operator types to finish a stalled upgrade.
const manualRestartHint = "systemctl restart ephemerd"

// triggerRestart asks systemd to restart the ephemerd unit into the
// newly-swapped binary.
//
// `--no-block` makes systemctl submit the restart job to systemd (PID 1)
// over D-Bus and return immediately, rather than waiting for the restart to
// complete. That matters because our process lives in the unit's cgroup:
// once systemd stops the unit it will kill everything in that cgroup,
// including this systemctl child. With --no-block the job is already queued
// with PID 1 the instant systemctl starts, so the restart proceeds even if
// the child is reaped a moment later. Setsid detaches it from our session
// as well. We Start (not Run) and never Wait — fire and forget.
//
// The two path arguments (the .old backup and the install path) exist for the
// Windows detached-helper hand-off and are unused here: systemd accepts a
// restart request from inside the unit, so there is nothing to hand off.
func triggerRestart(_, _ string) error {
	cmd := exec.Command("systemctl", "restart", "--no-block", serviceName)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("systemctl restart %s: %w", serviceName, err)
	}
	return nil
}
