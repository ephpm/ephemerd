package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runWorkflow(ctx, cmd.Args().First(), cmd.String("job"))
		},
	}
}

func runWorkflow(ctx context.Context, workflowPath string, jobFilter string) error {
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

	runner := &workflow.Runner{
		DataDir: configDir,
		Log:     log,
	}

	return runner.RunJob(ctx, jobName, job, repoDir)
}
