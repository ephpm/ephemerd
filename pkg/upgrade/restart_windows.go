//go:build windows

package upgrade

import (
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// serviceName is the Windows service created by cmd/ephemerd/install_windows.go.
const serviceName = "ephemerd"

// triggerRestart restarts the Windows service into the newly-swapped binary.
//
// A service cannot cleanly stop itself: calling `sc stop ephemerd` from
// inside the service process would ask the SCM to stop the very process
// making the call, and `sc start` immediately after races the stop. So we
// hand the whole stop→wait→start to a DETACHED process that outlives us.
// PowerShell's Restart-Service blocks until the service reaches STOPPED
// before starting it again, which removes the "service is still stopping"
// race. When it issues the stop, the SCM signals our svc.Handler, serve()
// returns, and this process exits — the detached PowerShell is not a child
// in any job tied to the service, so it survives to run the start, at which
// point the SCM launches the new bytes from the canonical (unchanged) path.
//
// DETACHED_PROCESS + CREATE_NEW_PROCESS_GROUP + HideWindow keep it fully
// decoupled and windowless. The brief sleep lets the RESTARTING progress
// message flush to the caller before the service goes down.
func triggerRestart() error {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"Start-Sleep -Seconds 2; Restart-Service -Force -Name "+serviceName)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("scheduling Restart-Service %s: %w", serviceName, err)
	}
	return nil
}
