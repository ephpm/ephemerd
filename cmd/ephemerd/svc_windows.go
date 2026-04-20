//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

const eventSource = "ephemerd"

// ephemerdService implements svc.Handler for the Windows Service Control Manager.
type ephemerdService struct {
	configFile     string
	ctrdTCPPort    uint32
	ctrdTCPAddr    string
	containerdOnly bool
	dind           bool
}

// Execute is called by the Windows SCM. It reports Running, starts serve()
// in a goroutine, and waits for a stop/shutdown signal from SCM.
func (s *ephemerdService) Execute(_ []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	// Open the Windows Event Log so all slog output goes there.
	elog, err := eventlog.Open(eventSource)
	if err != nil {
		// Can't log — report failure to SCM
		return false, 1
	}
	defer func() {
		if err := elog.Close(); err != nil {
			// Nothing we can do here
			_ = err
		}
	}()

	// Redirect the default slog logger to the Event Log. This captures
	// all output from ephemerd and containerd (which uses slog).
	slog.SetDefault(slog.New(slog.NewTextHandler(&eventLogWriter{elog: elog}, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run serve in a goroutine so we can report Running to SCM immediately.
	errCh := make(chan error, 1)
	go func() {
		errCh <- serve(ctx, s.configFile, s.ctrdTCPPort, s.ctrdTCPAddr, s.containerdOnly, s.dind)
	}()

	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case err := <-errCh:
			if err != nil {
				if logErr := elog.Error(1, fmt.Sprintf("ephemerd serve error: %v", err)); logErr != nil {
					fmt.Fprintf(os.Stderr, "ephemerd serve error: %v\n", err)
				}
				return false, 1
			}
			return false, 0

		case cr := <-r:
			switch cr.Cmd {
			case svc.Interrogate:
				status <- cr.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				if logErr := elog.Info(1, "ephemerd stopping"); logErr != nil {
					fmt.Fprintf(os.Stderr, "ephemerd stopping\n")
				}
				cancel() // triggers graceful drain in serve()
				if err := <-errCh; err != nil {
					if logErr := elog.Error(1, fmt.Sprintf("ephemerd shutdown error: %v", err)); logErr != nil {
						fmt.Fprintf(os.Stderr, "ephemerd shutdown error: %v\n", err)
					}
				}
				return false, 0
			}
		}
	}
}

// eventLogWriter implements io.Writer by sending each Write to the Windows
// Event Log. slog writes one log line per Write call, so each call becomes
// one event. Lines containing "level=ERROR" are logged as errors, "level=WARN"
// as warnings, everything else as info.
type eventLogWriter struct {
	elog *eventlog.Log
}

func (w *eventLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}

	switch {
	case strings.Contains(msg, "level=ERROR"):
		if err := w.elog.Error(1, msg); err != nil {
			return 0, err
		}
	case strings.Contains(msg, "level=WARN"):
		if err := w.elog.Warning(1, msg); err != nil {
			return 0, err
		}
	default:
		if err := w.elog.Info(1, msg); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// runAsWindowsService detects whether the process is running as a Windows
// service (non-interactive session) and if so, runs the SCM handler instead
// of the normal CLI. Returns true if it handled the invocation.
func runAsWindowsService() bool {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false
	}

	// When SCM starts the service, the command line is:
	//   "C:\Program Files\ephemerd\ephemerd.exe" serve --data-dir "C:\ProgramData\ephemerd"
	// Parse flags manually since we bypass urfave/cli.
	configFile := ""
	dataDir := defaultDataDir()
	for i, arg := range os.Args {
		if arg == "--data-dir" && i+1 < len(os.Args) {
			dataDir = os.Args[i+1]
		}
		if (arg == "--config" || arg == "-c") && i+1 < len(os.Args) {
			configFile = os.Args[i+1]
		}
	}
	configDir = dataDir

	if configFile == "" {
		configFile = joinPath(dataDir, "config.toml")
	}

	if err := svc.Run(eventSource, &ephemerdService{
		configFile: configFile,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "service run failed: %v\n", err)
		os.Exit(1)
	}

	return true
}

// installEventLog registers ephemerd as a Windows Event Log source.
// Called during `ephemerd install`.
func installEventLog() error {
	return eventlog.InstallAsEventCreate(eventSource, eventlog.Info|eventlog.Warning|eventlog.Error)
}

// removeEventLog removes the event log source. Called during `ephemerd uninstall`.
func removeEventLog() {
	_ = eventlog.Remove(eventSource)
}
