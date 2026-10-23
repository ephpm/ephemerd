//go:build linux

package runtime

import (
	"context"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/ephpm/ephemerd/pkg/runner"
)

// patchSnapshotRunner applies the LAN-cache patch (runner.PatchResultsURL)
// to the runner baked into an official runner image, inside this one
// container's writable snapshot. The write lands in the snapshot's upper
// layer, so the shared image is untouched and the patch matches whatever
// runner version the image ships. Called between NewContainer and NewTask,
// while nothing has the snapshot mounted.
func patchSnapshotRunner(ctx context.Context, sn snapshots.Snapshotter, key string) (bool, error) {
	mounts, err := sn.Mounts(ctx, key)
	if err != nil {
		return false, err
	}
	var patched bool
	err = mount.WithTempMount(ctx, mounts, func(root string) error {
		var perr error
		patched, perr = runner.PatchResultsURLFile(filepath.Join(root, officialRunnerRoot), "")
		return perr
	})
	return patched, err
}
