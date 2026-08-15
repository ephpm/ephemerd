package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli/v3"
)

// confirm prints prompt and reads a yes/no answer from stdin. Anything other
// than "y"/"yes" is a no — including a read error or EOF, which is what a
// non-interactive shell gives, so a destructive command invoked from a script
// or a cron job aborts instead of proceeding unattended.
//
// This is the single prompt used by every destructive command (cache clear,
// doctor cleanup, uninstall) so they all read and behave the same way.
func confirm(prompt string) bool {
	fmt.Print(prompt)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// actionPastTense renders a service action for the operator-facing line
// printed after it succeeds. Appending "ed" to the verb is what produced
// "ephemerd stoped" — the irregular forms are spelled out here instead.
func actionPastTense(action string) string {
	switch action {
	case "stop":
		return "stopped"
	case "start":
		return "started"
	case "restart":
		return "restarted"
	default:
		return action + "ed"
	}
}

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
