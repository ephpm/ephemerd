package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

func jobSSHCmd() *cli.Command {
	return &cli.Command{
		Name:      "ssh",
		Usage:     "SSH into a running macOS VM job (not available on Windows)",
		ArgsUsage: "<job-id>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return fmt.Errorf("jobs ssh is only available on macOS and Linux hosts")
		},
	}
}
