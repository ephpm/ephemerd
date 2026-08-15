package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/ephpm/ephemerd/pkg/upgrade"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
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
		// If SCM stop fails, fall back to killing the service's own process.
		if action == "stop" {
			return forceStopService(string(out))
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

// forceStopService is the last resort when the SCM refuses to stop the
// service. It kills the service's OWN process, by PID, as reported by the
// SCM.
//
// The previous fallback ran `taskkill /f /im ephemerd.exe`, which matches
// every process named ephemerd.exe on the host: an interactive
// `ephemerd run`, a second daemon, the containerd/dind helpers re-exec'd from
// the same image — and it SIGKILLs them all, taking every in-flight job with
// it, with a one-line "note:" as the only warning. Killing one known PID
// keeps the blast radius to the service the operator actually asked to stop.
//
// When the SCM cannot name the process, there is no safe kill left to make,
// so this refuses and hands the operator the exact command instead of
// guessing at a process name.
func forceStopService(scOut string) error {
	scOut = strings.TrimSpace(scOut)
	pid, err := serviceProcessID("ephemerd")
	if err != nil {
		return fmt.Errorf("sc stop failed (%s) and the service's PID could not be determined (%w); "+
			"refusing to kill every process named ephemerd.exe — that would kill in-flight jobs on "+
			"this node. Stop it from services.msc, or kill the specific process: taskkill /f /pid <pid>",
			scOut, err)
	}
	fmt.Printf("WARNING: sc stop failed: %s\n", scOut)
	fmt.Printf("WARNING: force-killing the ephemerd service process (pid %d). In-flight jobs are\n", pid)
	fmt.Println("WARNING: killed outright, not drained — use 'ephemerd drain --wait' to avoid this.")
	if killErr := exec.Command("taskkill", "/f", "/pid", strconv.FormatUint(uint64(pid), 10)).Run(); killErr != nil {
		return fmt.Errorf("sc stop failed (%s) and taskkill /pid %d failed: %w", scOut, pid, killErr)
	}
	fmt.Printf("ephemerd service process %d killed\n", pid)
	return nil
}

// serviceProcessID asks the SCM for the PID hosting the named service. A
// zero PID (the service is not running) is reported as an error so callers
// never kill PID 0.
func serviceProcessID(name string) (uint32, error) {
	m, err := mgr.Connect()
	if err != nil {
		return 0, fmt.Errorf("connecting to the service control manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()

	s, err := m.OpenService(name)
	if err != nil {
		return 0, fmt.Errorf("opening service %s: %w", name, err)
	}
	defer func() { _ = s.Close() }()

	st, err := s.Query()
	if err != nil {
		return 0, fmt.Errorf("querying service %s: %w", name, err)
	}
	if st.ProcessId == 0 {
		return 0, fmt.Errorf("service %s is not running (state %d)", name, st.State)
	}
	return st.ProcessId, nil
}

// stopServiceGraceful posts a STOP control to the SCM and returns as soon as
// it is accepted — it does NOT wait for the service to exit. That is exactly
// what the Windows drain path wants: the service handler cancels serve() and
// then keeps the SCM in StopPending while in-flight jobs finish (see
// svc_windows.go), which is the same graceful shutdown SIGTERM triggers on
// POSIX, and `drain` is documented to return immediately.
//
// A service that is already stopped, or already stopping, is success: the
// requested end state is the one already underway.
func stopServiceGraceful() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to the service control manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()

	s, err := m.OpenService("ephemerd")
	if err != nil {
		return fmt.Errorf("opening service ephemerd (is it installed as a service?): %w", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.Control(svc.Stop); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) ||
			errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) {
			return nil
		}
		return fmt.Errorf("asking the SCM to stop ephemerd: %w", err)
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
