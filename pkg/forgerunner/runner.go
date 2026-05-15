// Package forgerunner implements a lightweight Actions runner for Forgejo
// and Gitea. It registers with the forge via ConnectRPC, polls for tasks,
// and executes workflow steps via direct process spawning — no Docker.
//
// This package contains the shared logic. Platform-specific CLI entrypoints
// live in cmd/ephemerd-runner-forgejo (Forgejo) and cmd/ephemerd-runner-gitea (Gitea).
package forgerunner

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/ephpm/ephemerd/pkg/forgerpc"
)

// Config configures a forge runner instance.
type Config struct {
	// Platform identifies the forge type ("forgejo" or "gitea").
	Platform string

	// InstanceURL is the forge instance base URL (e.g., "https://codeberg.org").
	InstanceURL string

	// Token is the runner registration token from the forge admin panel.
	Token string

	// Name is the runner display name. Defaults to hostname.
	Name string

	// Labels are the runs-on labels to register (e.g., "ubuntu-latest").
	Labels []string

	// Version is reported to the forge during registration.
	Version string

	// HTTPClient is an optional *http.Client for the ConnectRPC client.
	// If nil, a default client with 30s timeout is used.
	HTTPClient *http.Client

	Log *slog.Logger
}

// Runner registers with a Forgejo or Gitea instance and executes workflow
// steps directly via process spawning — no Docker dependency.
type Runner struct {
	cfg    Config
	client *forgerpc.Client
	log    *slog.Logger
}

// New creates a forge runner from the given config.
func New(cfg Config) (*Runner, error) {
	if cfg.InstanceURL == "" {
		return nil, fmt.Errorf("forgerunner: instance URL is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("forgerunner: registration token is required")
	}
	if cfg.Name == "" {
		h, err := os.Hostname()
		if err != nil {
			cfg.Name = "ephemerd-runner"
		} else {
			cfg.Name = h
		}
	}
	if cfg.Version == "" {
		cfg.Version = "ephemerd-dev"
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if len(cfg.Labels) == 0 {
		cfg.Labels = []string{"ubuntu-latest"}
	}

	return &Runner{
		cfg:    cfg,
		client: forgerpc.NewClient(cfg.InstanceURL, cfg.HTTPClient),
		log:    cfg.Log,
	}, nil
}

// Run registers the runner, declares labels, and polls for a single task.
// It blocks until the task is executed or the context is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	runner, err := r.client.Register(ctx, r.cfg.Name, r.cfg.Token, r.cfg.Version, r.cfg.Labels)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	r.log.Info("registered",
		"platform", r.cfg.Platform,
		"id", runner.ID,
		"uuid", runner.UUID,
		"name", runner.Name,
	)

	if err := r.client.Declare(ctx, forgerpc.DeclareLabels(r.cfg.Labels)); err != nil {
		r.log.Warn("declare labels failed", "error", err)
	}

	return r.poll(ctx)
}

func (r *Runner) poll(ctx context.Context) error {
	var tasksVersion int64
	var failCount int

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		result, err := r.client.FetchTask(ctx, tasksVersion)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failCount++
			backoff := min(time.Duration(1<<uint(min(failCount, 6)))*time.Second, 60*time.Second)
			r.log.Warn("fetch task failed", "error", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
				continue
			}
		}
		failCount = 0
		tasksVersion = result.TasksVersion

		if result.Task == nil {
			continue
		}

		return r.execute(ctx, result.Task)
	}
}

func (r *Runner) execute(ctx context.Context, task *forgerpc.Task) error {
	r.log.Info("task received",
		"task_id", task.ID,
		"task_uuid", task.UUID,
		"repo", task.Repo(),
	)

	exec := NewExecutor(r.client, task, r.log)
	return exec.Run(ctx)
}
