package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/ephpm/ephemerd/pkg/upgrade"

	"github.com/urfave/cli/v3"
)

// restartHelperCmd is the hidden command a self-upgrading daemon spawns,
// detached, to restart the service it is itself running as.
//
// It exists because a Windows service cannot restart itself: the process that
// asks the SCM to stop the service is the process that goes away, leaving
// nobody to issue the start. Spawning `<ephemerd> __restart-service` hands
// stop → wait-for-STOPPED → start to a process that outlives the stop.
//
// It is deliberately boring: no config, no data-dir state beyond the log
// file, and nothing that can block. It talks to the SCM and exits. On failure
// it writes to the daemon's own log so `ephemerd logs` shows why an upgrade
// stalled, rather than the operator having to infer it from a version that
// never changed.
func restartHelperCmd() *cli.Command {
	return &cli.Command{
		Name:   upgrade.RestartHelperCommand,
		Usage:  "internal: restart the ephemerd service on behalf of a daemon that is being replaced",
		Hidden: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "service",
				Value: "ephemerd",
				Usage: "service (Windows) to restart",
			},
			&cli.DurationFlag{
				Name:  "stop-timeout",
				Value: upgrade.DefaultRestartStopTimeout,
				Usage: "give up if the service does not reach STOPPED within this long",
			},
			&cli.DurationFlag{
				Name:  "start-timeout",
				Value: upgrade.DefaultRestartStartTimeout,
				Usage: "give up if the service does not reach RUNNING within this long",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			log := restartHelperLogger()
			name := cmd.String("service")
			opts := upgrade.RestartOptions{
				StopTimeout:  cmd.Duration("stop-timeout"),
				StartTimeout: cmd.Duration("start-timeout"),
			}
			// Hard ceiling on the whole invocation so a wedged SCM can never
			// leave this process resident forever.
			ctx, cancel := context.WithTimeout(ctx, opts.StopTimeout+opts.StartTimeout+time.Minute)
			defer cancel()
			log.Info("restart helper: restarting service", "service", name,
				"stop_timeout", opts.StopTimeout, "start_timeout", opts.StartTimeout)
			if err := upgrade.RestartService(ctx, name, opts, log); err != nil {
				log.Error("restart helper: FAILED to restart the service — the upgraded binary is installed but not running",
					"service", name, "error", err, "remediation", upgrade.ManualRestartHint())
				return fmt.Errorf("restarting %s: %w", name, err)
			}
			log.Info("restart helper: service restarted", "service", name)
			return nil
		},
	}
}

// restartHelperLogger appends to the daemon's log file so the helper's
// account of the restart lands where an operator already looks. The daemon
// that spawned us is on its way out and will stop writing within seconds, so
// interleaving is not a concern. If the file cannot be opened we fall back to
// stderr, which for a detached helper is the null device — the restart still
// runs, it is just silent.
func restartHelperLogger() *slog.Logger {
	f, err := os.OpenFile(joinPath(configDir, "ephemerd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
