package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/vm"
	"github.com/ephpm/ephemerd/pkg/workflow"
	"github.com/urfave/cli/v3"
)

func runCmd() *cli.Command {
	return &cli.Command{
		Name:      "run",
		Usage:     "Run a GitHub Actions workflow locally",
		ArgsUsage: "[workflow-file]",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "job",
				Aliases: []string{"j"},
				Usage:   "run a specific job by name",
			},
			&cli.StringFlag{
				Name:  "image",
				Usage: "container image to use (default: from service config or built-in)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runWorkflow(ctx, cmd.Args().First(), cmd.String("job"), cmd.String("image"))
		},
	}
}

func runWorkflow(ctx context.Context, workflowPath string, jobFilter string, imageFlag string) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Find the workflow file
	if workflowPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getting working directory: %w", err)
		}
		found, err := workflow.FindWorkflow(cwd)
		if err != nil {
			return err
		}
		workflowPath = found
	}

	// Parse the workflow
	wf, err := workflow.Parse(workflowPath)
	if err != nil {
		return err
	}

	name := wf.Name
	if name == "" {
		name = workflowPath
	}
	fmt.Printf("Workflow: %s\n", name)

	// Select which job(s) to run
	var jobName string
	var job workflow.Job

	if jobFilter != "" {
		j, ok := wf.Jobs[jobFilter]
		if !ok {
			available := make([]string, 0, len(wf.Jobs))
			for k := range wf.Jobs {
				available = append(available, k)
			}
			return fmt.Errorf("job %q not found (available: %v)", jobFilter, available)
		}
		jobName = jobFilter
		job = j
	} else {
		// Run the first job (YAML map iteration order is non-deterministic,
		// but for a single-job workflow this is fine)
		for k, v := range wf.Jobs {
			jobName = k
			job = v
			break
		}
	}

	// Resolve repo directory (the directory containing .github/)
	repoDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Detect target platform and delegate Linux jobs to WSL on Windows
	platform := workflow.DetectPlatform(job.RunsOn)

	if platform == workflow.PlatformLinux && runtime.GOOS == "windows" {
		fmt.Printf("==> Job: %s (linux on windows — delegating to WSL)\n", jobName)

		distro, err := vm.NewRunDistro(ctx, vm.RunDistroConfig{
			DataDir: configDir,
			Log:     log,
		})
		if err != nil {
			return fmt.Errorf("setting up WSL run distro: %w", err)
		}
		defer distro.Destroy()

		absWorkflow, err := filepath.Abs(workflowPath)
		if err != nil {
			return fmt.Errorf("resolving workflow path: %w", err)
		}

		exitCode, err := distro.Run(ctx, vm.RunInWSLConfig{
			WorkflowPath: absWorkflow,
			JobFilter:    jobFilter,
			RepoDir:      repoDir,
		})
		if err != nil {
			return err
		}
		if exitCode != 0 {
			return fmt.Errorf("WSL ephemerd exited with code %d", exitCode)
		}
		return nil
	}

	// Use an isolated temp directory so the run command doesn't conflict with
	// the ephemerd service (BoltDB is single-writer and they'd share the same
	// data directory and named pipe otherwise).
	tmpDir, err := os.MkdirTemp("", "ephemerd-run-*")
	if err != nil {
		return fmt.Errorf("creating temp directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			log.Warn("failed to clean up temp directory", "dir", tmpDir, "error", err)
		}
	}()

	// On Windows, derive a unique named pipe so we don't collide with the
	// service's \\.\pipe\ephemerd-containerd. On Unix the socket lives
	// inside DataDir which is already unique.
	var socketPath string
	if runtime.GOOS == "windows" {
		socketPath = `\\.\pipe\ephemerd-run-` + filepath.Base(tmpDir)
	}

	image := resolveRunImage(imageFlag, platform)

	runner := &workflow.Runner{
		DataDir:    tmpDir,
		SocketPath: socketPath,
		Image:      image,
		Log:        log,
	}

	return runner.RunJob(ctx, jobName, job, repoDir)
}

// resolveRunImage determines the container image for a run job.
// Priority: --image flag → service config.toml → built-in default.
func resolveRunImage(flagValue string, platform workflow.TargetPlatform) string {
	if flagValue != "" {
		return flagValue
	}

	osName := platform.String()
	cfgPath := filepath.Join(configDir, "config.toml")
	if cfg, err := config.Load(cfgPath); err == nil {
		if img := cfg.GitHub.DefaultImageFor(osName); img != "" {
			return img
		}
	}

	return ""
}
