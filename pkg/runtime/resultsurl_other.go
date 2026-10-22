//go:build !linux

package runtime

import (
	"context"

	"github.com/containerd/containerd/v2/core/snapshots"
)

// patchSnapshotRunner: the LAN Actions cache is Linux-only (the scheduler
// never asks for it elsewhere), so there is nothing to patch.
func patchSnapshotRunner(context.Context, snapshots.Snapshotter, string) (bool, error) {
	return false, nil
}
