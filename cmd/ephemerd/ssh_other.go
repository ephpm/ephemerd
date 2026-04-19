//go:build !darwin

package main

import "github.com/urfave/cli/v3"

func jobSubcommands() []*cli.Command {
	return []*cli.Command{
		jobKillCmd(),
		jobLogsCmd(),
	}
}
