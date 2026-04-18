package main

import (
	"fmt"
	"os/exec"
)

const launchdPlist = "/Library/LaunchDaemons/dev.ephpm.ephemerd.plist"

func serviceAction(action string) error {
	switch action {
	case "start":
		out, err := exec.Command("launchctl", "load", "-w", launchdPlist).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl load: %s", out)
		}
	case "stop":
		out, err := exec.Command("launchctl", "unload", launchdPlist).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl unload: %s", out)
		}
	default:
		return fmt.Errorf("unsupported action: %s", action)
	}
	fmt.Printf("ephemerd %sed\n", action)
	return nil
}

func serviceLogs(lines int, follow bool) error {
	if follow {
		cmd := exec.Command("log", "stream", "--predicate", `subsystem == "dev.ephpm.ephemerd"`)
		cmd.Stdout = nil
		cmd.Stderr = nil
		return cmd.Run()
	}
	cmd := exec.Command("log", "show", "--predicate", `subsystem == "dev.ephpm.ephemerd"`, "--last", fmt.Sprintf("%dm", lines/10+1))
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}
