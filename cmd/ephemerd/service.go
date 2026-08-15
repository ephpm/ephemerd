package main

import (
	"context"

	"github.com/urfave/cli/v3"
)

func startCmd() *cli.Command {
	return &cli.Command{
		Name:  "start",
		Usage: "Start the ephemerd system service",
		Action: func(_ context.Context, _ *cli.Command) error {
			return serviceAction("start")
		},
	}
}

func stopCmd() *cli.Command {
	return &cli.Command{
		Name:  "stop",
		Usage: "Stop the ephemerd system service",
		Action: func(_ context.Context, _ *cli.Command) error {
			return serviceAction("stop")
		},
	}
}

func restartCmd() *cli.Command {
	return &cli.Command{
		Name:  "restart",
		Usage: "Restart the ephemerd system service",
		Action: func(_ context.Context, _ *cli.Command) error {
			return serviceRestart()
		},
	}
}

func logsCmd() *cli.Command {
	return &cli.Command{
		Name:  "logs",
		Usage: "Tail the ephemerd system service logs",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "lines",
				Value: 100,
				Usage: "number of lines to show",
			},
			&cli.BoolFlag{
				Name:    "follow",
				Aliases: []string{"f"},
				Usage:   "follow log output",
			},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			return serviceLogs(int(cmd.Int("lines")), cmd.Bool("follow"))
		},
	}
}
