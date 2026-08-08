//go:build darwin

package upgrade

import (
	"fmt"
	"os/exec"
	"syscall"
)

// serviceName is the launchd label from cmd/ephemerd/install_darwin.go.
const serviceName = "dev.ephpm.ephemerd"

// triggerRestart restarts the launchd daemon into the newly-swapped binary.
//
// The plist sets KeepAlive=true, so launchd would relaunch the daemon on any
// exit; `launchctl kickstart -k` makes that deterministic by killing and
// immediately restarting the service. Setsid + Start (no Wait) detaches it
// so it outlives this process when the daemon exits.
func triggerRestart() error {
	cmd := exec.Command("launchctl", "kickstart", "-k", "system/"+serviceName)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launchctl kickstart %s: %w", serviceName, err)
	}
	return nil
}
