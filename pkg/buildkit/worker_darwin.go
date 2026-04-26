//go:build darwin

package buildkit

import (
	"context"
	"fmt"

	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/worker"
)

// macOS jobs run inside the Linux VM, which hosts its own ephemerd with
// the Linux worker path. Native darwin doesn't need a buildkit worker.
func defaultSnapshotter() string { return "overlayfs" }

func newWorkerController(_ context.Context, _ Config, _ *session.Manager) (*worker.Controller, error) {
	return nil, fmt.Errorf("buildkit: native darwin has no worker; use the Linux VM path")
}
