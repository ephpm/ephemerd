package main

import (
	"fmt"
	"os/exec"
)

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
