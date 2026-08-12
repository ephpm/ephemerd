//go:build windows

package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// RestartService stops the named Windows service, waits for it to actually
// reach STOPPED, then starts it again — talking to the Service Control
// Manager directly.
//
// Doing the wait ourselves is the whole point. `sc.exe stop` is asynchronous,
// so "stop then start" without a wait races and fails with
// ERROR_SERVICE_CANNOT_ACCEPT_CTRL; and shelling out to PowerShell's
// Restart-Service (which does wait) costs an interpreter cold start in
// session 0 — the latency that made the v0.1.8 upgrade look like a failure.
// Two SCM calls and a poll loop cost neither.
//
// The outcome is reported honestly: a stop that never completes, or a start
// that never reaches RUNNING, is an error rather than a silently swallowed
// no-op, so the caller can say so instead of leaving an operator to infer it
// from a version that never changed.
func RestartService(ctx context.Context, name string, opts RestartOptions, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	opts = opts.withDefaults()

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to the service control manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("opening service %s: %w", name, err)
	}
	defer func() { _ = s.Close() }()

	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("querying service %s: %w", name, err)
	}

	if st.State != svc.Stopped {
		log.Info("restart: asking the SCM to stop the service", "service", name, "state", st.State)
		if _, err := s.Control(svc.Stop); err != nil && !stopAlreadyUnderway(err) {
			return fmt.Errorf("stopping service %s: %w", name, err)
		}
		if err := waitForState(ctx, s, svc.Stopped, opts.StopTimeout, opts.Poll); err != nil {
			return fmt.Errorf("waiting for %s to stop: %w", name, err)
		}
		log.Info("restart: service stopped", "service", name)
	}

	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("starting service %s: %w", name, err)
	}
	if err := waitForState(ctx, s, svc.Running, opts.StartTimeout, opts.Poll); err != nil {
		return fmt.Errorf("waiting for %s to start: %w", name, err)
	}
	log.Info("restart: service running", "service", name)
	return nil
}

// stopAlreadyUnderway reports whether a failed Control(Stop) merely means the
// service is already stopped or already stopping — in which case waiting for
// STOPPED is exactly the right next move, not an error.
func stopAlreadyUnderway(err error) bool {
	return errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) ||
		errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL)
}

// waitForState polls the SCM until the service reaches want, or timeout
// elapses. The state actually observed goes into the error, so a caller's log
// says "still StopPending" rather than just "timed out".
func waitForState(ctx context.Context, s *mgr.Service, want svc.State, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("querying service: %w", err)
		}
		if st.State == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s (state is %d, want %d)", timeout, st.State, want)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
