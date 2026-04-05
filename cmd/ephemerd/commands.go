package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	containerdpkg "github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/scheduler"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// controlClient dials the daemon's gRPC unix socket and returns a client.
func controlClient(ctx context.Context) (apiv1.ControlClient, *grpc.ClientConn, error) {
	sock := scheduler.SocketPath(configDir)
	conn, err := grpc.NewClient("unix:"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot connect to ephemerd at %s (is it running?): %w", sock, err)
	}
	return apiv1.NewControlClient(conn), conn, nil
}

func statusCmd() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "Show running jobs and daemon health",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cc, conn, err := controlClient(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			resp, err := cc.Status(ctx, &apiv1.StatusRequest{})
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}

			data := map[string]any{
				"status":         resp.Status,
				"active_jobs":    resp.ActiveJobs,
				"max_concurrent": resp.MaxConcurrent,
				"draining":       resp.Draining,
				"uptime":         resp.Uptime,
			}

			pretty, _ := json.MarshalIndent(data, "", "  ")
			fmt.Println(string(pretty))
			return nil
		},
	}
}

func drainCmd() *cli.Command {
	return &cli.Command{
		Name:        "drain",
		Usage:       "Stop accepting new jobs and wait for running jobs to finish",
		Description: "Sends SIGTERM to the running ephemerd daemon, triggering graceful drain.\nThe daemon will stop accepting new jobs and wait for running jobs to complete.",
		Action: func(ctx context.Context, cmd *cli.Command) error {
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
			cc, conn, grpcErr := controlClient(ctx)
			if grpcErr == nil {
				defer conn.Close()
				resp, err := cc.Status(ctx, &apiv1.StatusRequest{})
				if err == nil {
					fmt.Printf("Active jobs: %d\n", resp.ActiveJobs)
				}
			}

			fmt.Printf("Sending SIGTERM to ephemerd (pid %d)...\n", pid)
			if err := proc.Signal(syscall.SIGTERM); err != nil {
				return fmt.Errorf("failed to signal process %d: %w", pid, err)
			}

			fmt.Println("The daemon will wait for running jobs to finish before exiting.")
			fmt.Println("Use 'ephemerd status' to monitor progress.")
			return nil
		},
	}
}

func jobsCmd() *cli.Command {
	return &cli.Command{
		Name:      "jobs",
		Usage:     "List and manage running jobs",
		ArgsUsage: "[job-id]",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cc, conn, err := controlClient(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			// If a job ID argument is given, show that job's details
			if cmd.Args().Len() > 0 {
				return jobInspect(ctx, cc, cmd.Args().First())
			}

			return jobList(ctx, cc)
		},
		Commands: []*cli.Command{
			jobKillCmd(),
			jobLogsCmd(),
		},
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
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() == 0 {
				return fmt.Errorf("job ID required")
			}

			jobID, err := strconv.ParseInt(cmd.Args().First(), 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job id: %w", err)
			}

			cc, conn, err := controlClient(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

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
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() == 0 {
				return fmt.Errorf("job ID required")
			}

			jobID, err := strconv.ParseInt(cmd.Args().First(), 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job id: %w", err)
			}

			cc, conn, err := controlClient(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			stream, err := cc.GetJobLogs(ctx, &apiv1.GetJobLogsRequest{Id: jobID})
			if err != nil {
				return fmt.Errorf("get logs: %w", err)
			}

			for {
				chunk, err := stream.Recv()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return fmt.Errorf("reading logs: %w", err)
				}
				os.Stdout.Write(chunk.Data)
			}
		},
	}
}

func imagesCmd() *cli.Command {
	return &cli.Command{
		Name:  "images",
		Usage: "List cached container images",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			socket := containerdpkg.SocketPath(configDir)
			c, err := client.New(socket)
			if err != nil {
				return fmt.Errorf("connecting to containerd at %s: %w", socket, err)
			}
			defer func() { _ = c.Close() }()

			nsCtx := namespaces.WithNamespace(ctx, "ephemerd")
			images, err := c.ListImages(nsCtx)
			if err != nil {
				return fmt.Errorf("listing images: %w", err)
			}

			if len(images) == 0 {
				fmt.Println("No cached images.")
				return nil
			}

			fmt.Printf("%-60s %s\n", "IMAGE", "SIZE")
			for _, img := range images {
				size, _ := img.Size(nsCtx)
				fmt.Printf("%-60s %s\n", img.Name(), formatBytes(size))
			}

			return nil
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

			if cfg.GitHub.TLSCert != "" {
				fmt.Printf("  Mode:            webhook (TLS)\n")
				fmt.Printf("  Webhook port:    %d\n", cfg.GitHub.WebhookPort)
			} else {
				fmt.Printf("  Mode:            polling\n")
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

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
