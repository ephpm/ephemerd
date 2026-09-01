//go:build !linux && !windows

package runnerbusy

import (
	"context"
	"log/slog"
)

// ContainerBusy has no implementation on this platform.
//
// The only non-Linux, non-Windows host ephemerd runs on is macOS, and a
// macOS host never owns a runner container directly: Linux jobs there are
// dispatched into the Linux sidecar VM, whose containerd lives on the far
// side of a gRPC boundary, and macOS jobs run in a per-job macOS VM
// probed over SSH instead (see the scheduler's macOS VM prober).
//
// This returns Unknown + ErrUnsupported so the caller falls through to
// the next authority rather than mistaking "no probe" for "not busy".
func ContainerBusy(_ context.Context, _ ContainerTask, _ *slog.Logger) (State, error) {
	return Unknown, ErrUnsupported
}
