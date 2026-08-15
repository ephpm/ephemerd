package main

import (
	"fmt"
	"os"
	"os/exec"
)

// serviceRestart restarts the unit. systemd's own restart verb already
// sequences stop-then-start correctly, so there is nothing to add here.
func serviceRestart() error { return serviceAction("restart") }

func serviceAction(action string) error {
	out, err := exec.Command("systemctl", action, "ephemerd").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %s", action, out)
	}
	fmt.Printf("ephemerd %s\n", actionPastTense(action))
	return nil
}

// stopServiceGraceful exists so the control-socket drain path compiles on
// every platform; only Windows uses it (see drainViaControl). On Linux the
// default drain signals the daemon directly, which is both graceful and
// independent of systemd, so there is nothing for this to do.
func stopServiceGraceful() error {
	return fmt.Errorf("stopping the service through a service manager is only used on Windows")
}

func serviceLogs(lines int, follow bool) error {
	args := []string{"-u", "ephemerd", "-n", fmt.Sprintf("%d", lines), "--no-pager"}
	if follow {
		args = append(args, "-f")
	}
	cmd := exec.Command("journalctl", args...)
	// journalctl writes the log to its own stdout. Leaving these nil sends
	// both streams to /dev/null, which is what made `ephemerd logs` exit 0
	// having printed absolutely nothing.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
