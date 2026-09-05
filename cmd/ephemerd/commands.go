package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/scheduler"

	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// controlConn holds a gRPC connection and its client for the daemon control socket.
type controlConn struct {
	apiv1.ControlClient
	conn *grpc.ClientConn
}

// Close closes the underlying gRPC connection.
func (c *controlConn) Close() error {
	return c.conn.Close()
}

// dialControl connects to the daemon's gRPC unix socket.
func dialControl(ctx context.Context) (*controlConn, error) {
	sock := scheduler.SocketPath(configDir)
	conn, err := grpc.NewClient("unix:"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to ephemerd at %s (is it running?): %w", sock, err)
	}
	return &controlConn{
		ControlClient: apiv1.NewControlClient(conn),
		conn:          conn,
	}, nil
}

func statusCmd() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "Show running jobs and daemon health",
		Action: func(ctx context.Context, cmd *cli.Command) (retErr error) {
			cc, err := dialControl(ctx)
			if err != nil {
				return err
			}
			defer func() {
				if err := cc.Close(); err != nil && retErr == nil {
					retErr = fmt.Errorf("closing connection: %w", err)
				}
			}()

			resp, err := cc.Status(ctx, &apiv1.StatusRequest{})
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}

			// held_slots is deliberately next to active_jobs: they differ
			// exactly when a job is provisioning or a slot leaked, and a
			// node whose slots are all held while active_jobs reads 0 is
			// the shape of the 28-hour macOS stall in issue #196.
			slots := make([]map[string]any, 0, len(resp.SlotPools))
			for _, p := range resp.SlotPools {
				slots = append(slots, map[string]any{
					"pool":     p.Pool,
					"held":     p.Held,
					"capacity": p.Capacity,
				})
			}

			data := map[string]any{
				"status":         resp.Status,
				"active_jobs":    resp.ActiveJobs,
				"max_concurrent": resp.MaxConcurrent,
				"held_slots":     resp.HeldSlots,
				"slot_capacity":  resp.SlotCapacity,
				"slots":          slots,
				"draining":       resp.Draining,
				"uptime":         resp.Uptime,
				"version":        resp.Version,
			}

			pretty, _ := json.MarshalIndent(data, "", "  ")
			fmt.Println(string(pretty))
			return nil
		},
	}
}

// drainStrategy is how `ephemerd drain` (without --wait) tells the running
// daemon to stop claiming jobs.
type drainStrategy int

const (
	// drainSignal sends SIGTERM to the PID in the pid file: the daemon stops
	// claiming, lets in-flight jobs finish, then exits.
	drainSignal drainStrategy = iota
	// drainControl cordons over the control socket and then asks the SCM to
	// stop the service, which runs that same graceful shutdown.
	drainControl
)

// drainStrategyFor picks the drain mechanism for a platform.
//
// Windows has no signals: os.Process.Signal always fails with "not supported
// by windows" there, so the SIGTERM path could never do anything but error —
// `ephemerd drain` was simply broken on every Windows node. Windows gets the
// control-socket route instead, which reaches the same end state through the
// SCM. Pure, so the routing rule is testable without a daemon.
func drainStrategyFor(goos string) drainStrategy {
	if goos == "windows" {
		return drainControl
	}
	return drainSignal
}

func drainCmd() *cli.Command {
	return &cli.Command{
		Name:  "drain",
		Usage: "Stop accepting new jobs and wait for running jobs to finish",
		Description: "By default sends SIGTERM to the running ephemerd daemon: it stops claiming new jobs,\n" +
			"keeps in-flight jobs running until they finish (or shutdown_timeout expires, default 5m),\n" +
			"then exits. This command returns immediately; use 'ephemerd status' to monitor progress.\n" +
			"\n" +
			"On Windows there are no signals, so the default path instead cordons the daemon over the\n" +
			"control socket and asks the Service Control Manager to stop it — the service handler runs\n" +
			"the same graceful shutdown, waiting for in-flight jobs. It also returns immediately.\n" +
			"\n" +
			"With --wait no signal is sent. The daemon is cordoned over the control socket (it stops\n" +
			"claiming new jobs but keeps serving) and this command polls until the active job count\n" +
			"reaches zero or --timeout elapses (nonzero exit). Once drained, the daemon can be\n" +
			"restarted without killing a single job, e.g.:\n" +
			"\n" +
			"    ephemerd drain --wait && systemctl restart ephemerd",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "wait",
				Usage: "cordon via the control socket (no signal) and block until all running jobs finish",
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Value: 45 * time.Minute,
				Usage: "with --wait: give up after this long (exits nonzero, daemon stays cordoned)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Bool("wait") {
				return drainWait(ctx, cmd.Duration("timeout"))
			}
			if drainStrategyFor(runtime.GOOS) == drainControl {
				return drainViaControl(ctx)
			}
			return drainViaSignal(ctx)
		},
	}
}

// drainViaSignal is the POSIX default drain: SIGTERM the daemon, which stops
// claiming, finishes in-flight jobs, then exits. Returns immediately.
func drainViaSignal(ctx context.Context) error {
	// Read PID file to find the running daemon
	pidFile := filepath.Join(configDir, "ephemerd.pid")
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		return fmt.Errorf("cannot read pid file %s (is ephemerd running?): %w", pidFile, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		return fmt.Errorf("invalid pid file: %w", err)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("cannot find process %d: %w", pid, err)
	}

	// Check current status via gRPC if reachable
	cc, dialErr := dialControl(ctx)
	if dialErr == nil {
		resp, err := cc.Status(ctx, &apiv1.StatusRequest{})
		if err == nil {
			fmt.Printf("Active jobs: %d\n", resp.ActiveJobs)
		}
		if err := cc.Close(); err != nil {
			return fmt.Errorf("closing connection: %w", err)
		}
	}

	fmt.Printf("Sending SIGTERM to ephemerd (pid %d)...\n", pid)
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to signal process %d: %w", pid, err)
	}

	fmt.Println("The daemon will wait for running jobs to finish before exiting.")
	fmt.Println("Use 'ephemerd status' to monitor progress.")
	return nil
}

// drainViaControl is the Windows default drain. There is no SIGTERM to send,
// so the daemon is cordoned over the control socket — it stops claiming new
// jobs the instant that returns — and then the SCM is asked to stop the
// service, whose handler cancels serve() and holds StopPending while
// in-flight jobs finish. Same end state as the POSIX signal, and like the
// signal it returns immediately rather than blocking.
//
// The cordon comes first on purpose: it is the half that matters most and it
// works whether or not ephemerd was installed as a service. If the SCM stop
// then fails (running in the foreground, no service registered, no rights),
// the node is still claiming nothing and the operator is told how to finish;
// that is reported as success because the documented job of `drain` — stop
// accepting new work — is done.
func drainViaControl(ctx context.Context) (retErr error) {
	cc, err := dialControl(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := cc.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("closing connection: %w", err)
		}
	}()

	resp, err := cc.Cordon(ctx, &apiv1.CordonRequest{})
	if err != nil {
		return fmt.Errorf("cordon: %w", err)
	}
	fmt.Printf("Cordoned: daemon stopped claiming new jobs (%d active).\n", resp.ActiveJobs)

	if err := stopServiceGraceful(); err != nil {
		fmt.Printf("Could not ask the service manager to stop ephemerd: %v\n", err)
		fmt.Println("The daemon stays cordoned, so it is claiming nothing. Run 'ephemerd drain --wait'")
		fmt.Println("to block until in-flight jobs finish, or 'ephemerd uncordon' to resume claiming.")
		return nil
	}

	fmt.Println("Asked the service manager to stop ephemerd; it exits once running jobs finish.")
	fmt.Println("Use 'ephemerd status' to monitor progress.")
	return nil
}

// drainWait cordons the daemon over the control socket — no signal, so the
// daemon keeps running — then polls Status until no jobs remain or the
// timeout elapses. Exit 0 means drained: a restart at that point kills
// nothing. On timeout the daemon is left cordoned so the operator can
// decide: restart anyway, or `ephemerd uncordon` to resume claiming.
func drainWait(ctx context.Context, timeout time.Duration) (retErr error) {
	cc, err := dialControl(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := cc.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("closing connection: %w", err)
		}
	}()

	resp, err := cc.Cordon(ctx, &apiv1.CordonRequest{})
	if err != nil {
		return fmt.Errorf("cordon: %w", err)
	}
	fmt.Printf("Cordoned: daemon stopped claiming new jobs (%d active).\n", resp.ActiveJobs)
	if resp.ActiveJobs == 0 {
		fmt.Println("Drained: no active jobs.")
		return nil
	}

	start := time.Now()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("interrupted while waiting for drain (daemon stays cordoned): %w", ctx.Err())
		case <-ticker.C:
			st, err := cc.Status(ctx, &apiv1.StatusRequest{})
			if err != nil {
				return fmt.Errorf("status while draining: %w", err)
			}
			if st.ActiveJobs == 0 {
				fmt.Printf("Drained: no active jobs (waited %s).\n", time.Since(start).Truncate(time.Second))
				return nil
			}
			fmt.Printf("Waiting: %d active job(s), elapsed %s\n", st.ActiveJobs, time.Since(start).Truncate(time.Second))
			if time.Now().After(deadline) {
				return fmt.Errorf("drain timed out after %s with %d job(s) still running (daemon stays cordoned)", timeout, st.ActiveJobs)
			}
		}
	}
}

func uncordonCmd() *cli.Command {
	return &cli.Command{
		Name:        "uncordon",
		Usage:       "Resume claiming new jobs after a cordon or aborted drain",
		Description: "Clears the draining flag set by 'ephemerd drain --wait' (or a timed-out drain),\nso the daemon starts claiming queued jobs again. Running jobs are unaffected.",
		Action: func(ctx context.Context, cmd *cli.Command) (retErr error) {
			cc, err := dialControl(ctx)
			if err != nil {
				return err
			}
			defer func() {
				if err := cc.Close(); err != nil && retErr == nil {
					retErr = fmt.Errorf("closing connection: %w", err)
				}
			}()

			resp, err := cc.Uncordon(ctx, &apiv1.UncordonRequest{})
			if err != nil {
				return fmt.Errorf("uncordon: %w", err)
			}
			fmt.Printf("Uncordoned: daemon is claiming new jobs again (%d active).\n", resp.ActiveJobs)
			return nil
		},
	}
}

func jobsCmd() *cli.Command {
	return &cli.Command{
		Name:      "jobs",
		Usage:     "List and manage running jobs",
		ArgsUsage: "[job-id]",
		Action: func(ctx context.Context, cmd *cli.Command) (retErr error) {
			cc, err := dialControl(ctx)
			if err != nil {
				return err
			}
			defer func() {
				if err := cc.Close(); err != nil && retErr == nil {
					retErr = fmt.Errorf("closing connection: %w", err)
				}
			}()

			// If a job ID argument is given, show that job's details
			if cmd.Args().Len() > 0 {
				return jobInspect(ctx, cc, cmd.Args().First())
			}

			return jobList(ctx, cc)
		},
		Commands: jobSubcommands(),
	}
}

func jobList(ctx context.Context, cc apiv1.ControlClient) error {
	resp, err := cc.ListJobs(ctx, &apiv1.ListJobsRequest{})
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}

	if len(resp.Jobs) == 0 {
		fmt.Println("No running jobs.")
		return nil
	}

	fmt.Printf("%-14s %-40s %-25s %-10s %s\n", "JOB ID", "NAME", "REPO", "STATUS", "UPTIME")
	for _, j := range resp.Jobs {
		fmt.Printf("%-14d %-40s %-25s %-10s %s\n",
			j.Id, j.Name, j.Repo, j.Status, j.Uptime)
	}
	return nil
}

func jobInspect(ctx context.Context, cc apiv1.ControlClient, jobIDStr string) error {
	jobID, err := strconv.ParseInt(jobIDStr, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job id %q: %w", jobIDStr, err)
	}

	job, err := cc.GetJob(ctx, &apiv1.GetJobRequest{Id: jobID})
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}

	data := map[string]any{
		"id":         job.Id,
		"name":       job.Name,
		"repo":       job.Repo,
		"image":      job.Image,
		"runner_id":  job.RunnerId,
		"status":     job.Status,
		"pid":        job.Pid,
		"started_at": job.StartedAt,
		"uptime":     job.Uptime,
	}

	pretty, _ := json.MarshalIndent(data, "", "  ")
	fmt.Println(string(pretty))
	return nil
}

func jobKillCmd() *cli.Command {
	return &cli.Command{
		Name:      "kill",
		Usage:     "Kill a running job",
		ArgsUsage: "<job-id>",
		Action: func(ctx context.Context, cmd *cli.Command) (retErr error) {
			if cmd.Args().Len() == 0 {
				return fmt.Errorf("job ID required")
			}

			jobID, err := strconv.ParseInt(cmd.Args().First(), 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job id: %w", err)
			}

			cc, err := dialControl(ctx)
			if err != nil {
				return err
			}
			defer func() {
				if err := cc.Close(); err != nil && retErr == nil {
					retErr = fmt.Errorf("closing connection: %w", err)
				}
			}()

			if _, err := cc.KillJob(ctx, &apiv1.KillJobRequest{Id: jobID}); err != nil {
				return fmt.Errorf("kill job: %w", err)
			}

			fmt.Printf("Job %d killed.\n", jobID)
			return nil
		},
	}
}

func jobLogsCmd() *cli.Command {
	return &cli.Command{
		Name:      "logs",
		Usage:     "Show logs for a running job",
		ArgsUsage: "<job-id>",
		Action: func(ctx context.Context, cmd *cli.Command) (retErr error) {
			if cmd.Args().Len() == 0 {
				return fmt.Errorf("job ID required")
			}

			jobID, err := strconv.ParseInt(cmd.Args().First(), 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job id: %w", err)
			}

			cc, err := dialControl(ctx)
			if err != nil {
				return err
			}
			defer func() {
				if err := cc.Close(); err != nil && retErr == nil {
					retErr = fmt.Errorf("closing connection: %w", err)
				}
			}()

			stream, err := cc.GetJobLogs(ctx, &apiv1.GetJobLogsRequest{Id: jobID})
			if err != nil {
				return fmt.Errorf("get logs: %w", err)
			}

			for {
				chunk, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return fmt.Errorf("reading logs: %w", err)
				}
				if _, err := os.Stdout.Write(chunk.Data); err != nil {
					return fmt.Errorf("writing logs: %w", err)
				}
			}
		},
	}
}

func configCheckCmd() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "Validate configuration file",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Usage:   "path to config file",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			configFile := cmd.String("config")
			if configFile == "" {
				configFile = filepath.Join(configDir, "config.toml")
			}

			cfg, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("invalid config: %w", err)
			}

			fmt.Printf("Config: %s\n", configFile)
			fmt.Printf("  GitHub owner:    %s\n", cfg.GitHub.Owner)
			fmt.Printf("  Repos:           %v\n", cfg.GitHub.Repos)
			fmt.Printf("  Max concurrent:  %d\n", cfg.Runner.MaxConcurrent)
			fmt.Printf("  Job timeout:     %s\n", cfg.Runner.JobTimeout)
			fmt.Printf("  Poll interval:   %s\n", cfg.GitHub.PollInterval)
			fmt.Printf("  Log level:       %s\n", cfg.Log.Level)

			if cfg.Webhook.Tunnel == "none" {
				fmt.Printf("  Mode:            polling\n")
			} else if cfg.Webhook.TLSCert != "" {
				fmt.Printf("  Mode:            webhook (TLS)\n")
				fmt.Printf("  Webhook port:    %d\n", cfg.Webhook.Port)
			} else {
				fmt.Printf("  Mode:            webhook (tunnel: %s)\n", cfg.Webhook.Tunnel)
			}

			if cfg.GitHub.Token != "" {
				fmt.Printf("  Auth:            token (set)\n")
			} else if cfg.GitHub.AppID != 0 {
				fmt.Printf("  Auth:            GitHub App (ID: %d)\n", cfg.GitHub.AppID)
			}

			fmt.Println("\nConfig OK")
			return nil
		},
	}
}
