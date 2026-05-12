//go:build !linux

package main

import (
	"context"
	"log/slog"

	"github.com/containerd/containerd/v2/client"
)

// startWorkerDebugExec is a no-op on non-Linux because the worker mode
// (containerd-only, dispatched into) only runs in the Linux VM.
func startWorkerDebugExec(_ context.Context, _ int, _ *client.Client, _ *slog.Logger) func() {
	return func() {}
}
