//go:build windows

package upgrade

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// serviceName is the Windows service created by cmd/ephemerd/install_windows.go.
const serviceName = "ephemerd"

// manualRestartHint is what an operator types to finish a stalled upgrade.
const manualRestartHint = "Restart-Service ephemerd -Force"

// triggerRestart hands the restart to a DETACHED helper that outlives this
// process, because a service cannot stop and then start itself: the moment
// the SCM accepts the stop, the process making the call is the one going
// away, so nothing is left to issue the start.
//
// The helper is `<ephemerd> __restart-service`, a hidden CLI command that
// talks to the SCM directly (see RestartService): stop, wait for STOPPED,
// start. That replaces the previous `powershell -Command "Restart-Service"`
// hand-off, which was correct in principle but far too heavy in practice. On
// the fleet's Windows runner, the gap between the swap and the SCM stop
// measured 6m34s — a cold powershell.exe start in session 0 as LocalSystem,
// immediately after ~1 GB of download/extract/delete I/O and an AV scan of
// the freshly written binary — while the CLI gives up after minutes. The
// upgrade had in fact worked; it just reported failure, and left the node
// cordoned in the meantime.
//
// Two deliberate choices keep the helper cheap and correct:
//
//   - It is spawned from helperPath, the backup of the CURRENTLY RUNNING
//     image (ephemerd.exe.old after the swap), not from the newly installed
//     binary. That image is already resident and already AV-scanned, so it
//     starts immediately, and — more importantly — it is by definition the
//     same build as the code constructing this argv, so the helper's command
//     contract cannot be out of sync with its caller. installPath is the
//     fallback if the backup is missing.
//   - No sc.exe, no shell, no PATH lookup: SCM calls only.
//
// DETACHED_PROCESS + CREATE_NEW_PROCESS_GROUP + HideWindow keep it fully
// decoupled and windowless. Windows services are not job-confined by default,
// so the helper survives its parent's exit.
//
// Starting the helper is NOT proof the restart happened. superviseRestart
// watches for it to take effect and un-cordons the node if it does not.
func triggerRestart(helperPath, installPath string) error {
	exe := helperPath
	if exe == "" {
		exe = installPath
	} else if _, err := os.Stat(exe); err != nil {
		exe = installPath
	}

	cmd := exec.Command(exe, RestartHelperCommand, "--service", serviceName)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawning detached restart helper %s: %w", exe, err)
	}
	// We never Wait: the helper is meant to outlive us. Release the process
	// handle so nothing leaks during the seconds we have left.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("releasing restart helper handle: %w", err)
	}
	return nil
}
