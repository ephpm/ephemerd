package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ephpm/ephemerd/pkg/forgerunner"
	"github.com/urfave/cli/v3"
)

var version = "dev"

func main() {
	app := &cli.Command{
		Name:    "gitea-runner",
		Usage:   "Ephemeral Gitea Actions runner — direct process execution, no Docker",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "instance",
				Usage:   "Gitea instance URL",
				Sources: cli.EnvVars("GITEA_INSTANCE_URL"),
			},
			&cli.StringFlag{
				Name:    "token",
				Usage:   "runner registration token",
				Sources: cli.EnvVars("GITEA_REG_TOKEN"),
			},
			&cli.StringFlag{
				Name:    "name",
				Usage:   "runner display name (default: hostname)",
				Sources: cli.EnvVars("GITEA_RUNNER_NAME"),
			},
			&cli.StringSliceFlag{
				Name:    "label",
				Usage:   "runs-on label (can be repeated)",
				Value:   []string{"ubuntu-latest"},
				Sources: cli.EnvVars("GITEA_RUNNER_LABELS"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			r, err := forgerunner.New(forgerunner.Config{
				Platform:    "gitea",
				InstanceURL: cmd.String("instance"),
				Token:       cmd.String("token"),
				Name:        cmd.String("name"),
				Labels:      cmd.StringSlice("label"),
				Version:     version,
				Log:         slog.Default(),
			})
			if err != nil {
				return err
			}
			return r.Run(ctx)
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
