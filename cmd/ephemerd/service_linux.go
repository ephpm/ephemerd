package main

import (
	"fmt"
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
	fmt.Printf("ephemerd %sed\n", action)
	return nil
}

func serviceLogs(lines int, follow bool) error {
	args := []string{"-u", "ephemerd", "-n", fmt.Sprintf("%d", lines), "--no-pager"}
	if follow {
		args = append(args, "-f")
	}
	cmd := exec.Command("journalctl", args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}
