//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
)

const eventSource = "ephemerd"

// serviceLogWriter is set by the Windows Service handler before calling
// serve(). When non-nil, serve() injects it into the config so cfg.Logger()
// routes output to the log file instead of stderr.
var serviceLogWriter io.Writer

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

	// Open a log file in the data directory for service output.
	logPath := joinPath(configDir, "ephemerd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false, 1
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			slog.Warn("closing service log file", "error", err)
		}
	}()

	serviceLogWriter = logFile
	slog.SetDefault(slog.New(slog.NewTextHandler(logFile, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- serve(ctx, s.configFile, "", s.ctrdTCPPort, s.ctrdTCPAddr, s.containerdOnly, s.dind, false)
	}()

	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case err := <-errCh:
			if err != nil {
				if _, wErr := fmt.Fprintf(logFile, "ephemerd serve error: %v\n", err); wErr != nil {
					slog.Warn("writing to service log", "error", wErr)
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
				if _, wErr := fmt.Fprintln(logFile, "ephemerd stopping"); wErr != nil {
					slog.Warn("writing to service log", "error", wErr)
				}
				cancel()
				// Wait for serve() to exit, but force-exit after 30s
				// to avoid hanging the SCM indefinitely.
				select {
				case err := <-errCh:
					if err != nil {
						if _, wErr := fmt.Fprintf(logFile, "ephemerd shutdown error: %v\n", err); wErr != nil {
							slog.Warn("writing to service log", "error", wErr)
						}
					}
				case <-time.After(30 * time.Second):
					if _, wErr := fmt.Fprintln(logFile, "ephemerd shutdown timed out after 30s, force exiting"); wErr != nil {
						slog.Warn("writing to service log", "error", wErr)
					}
				}
				return false, 0
			}
		}
	}
}

// getServiceLogWriter returns the log file writer if running as a service.
func getServiceLogWriter() io.Writer {
	return serviceLogWriter
}

// runAsWindowsService detects whether the process is running as a Windows
// service (non-interactive session) and if so, runs the SCM handler instead
// of the normal CLI. Returns true if it handled the invocation.
func runAsWindowsService() bool {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false
	}

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
