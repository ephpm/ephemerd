package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"github.com/ephpm/ephemerd/pkg/upgrade"
)

// serviceRestart restarts the service through the SCM directly: stop, WAIT
// for STOPPED, then start.
//
// `ephemerd restart` used to issue `sc.exe stop` immediately followed by
// `sc.exe start`, but `sc stop` only posts the control and returns — so the
// start raced the stop and failed with ERROR_SERVICE_CANNOT_ACCEPT_CTRL
// whenever the daemon took more than an instant to shut down. It drains jobs
// and containerd on the way out, so it always does.
func serviceRestart() error {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := upgrade.RestartService(context.Background(), "ephemerd", upgrade.RestartOptions{}, log); err != nil {
		return fmt.Errorf("restarting ephemerd: %w", err)
	}
	fmt.Println("ephemerd restarted")
	return nil
}

func serviceAction(action string) error {
	out, err := exec.Command("sc.exe", action, "ephemerd").CombinedOutput()
	if err != nil {
		// If SCM stop fails, force kill the process
		if action == "stop" {
			fmt.Printf("note: sc stop failed, force killing: %s", string(out))
			if killErr := exec.Command("taskkill", "/f", "/im", "ephemerd.exe").Run(); killErr != nil {
				return fmt.Errorf("sc stop and taskkill both failed: %s", string(out))
			}
			fmt.Println("ephemerd killed")
			return nil
		}
		return fmt.Errorf("sc %s: %s", action, out)
	}
	switch action {
	case "stop":
		fmt.Println("ephemerd stopped")
	case "start":
		fmt.Println("ephemerd started")
	default:
		fmt.Printf("ephemerd %s complete\n", action)
	}
	return nil
}

func serviceLogs(lines int, follow bool) error {
	logPath := joinPath(configDir, "ephemerd.log")

	if follow {
		// Use PowerShell Get-Content -Wait for tail -f equivalent
		cmd := exec.Command("powershell", "-NoProfile", "-Command",
			fmt.Sprintf("Get-Content -Path '%s' -Tail %d -Wait", logPath, lines))
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Read last N lines
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		fmt.Sprintf("Get-Content -Path '%s' -Tail %d", logPath, lines))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
