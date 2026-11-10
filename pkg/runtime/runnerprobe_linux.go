//go:build linux

package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/ephpm/ephemerd/pkg/runnerversion"
	"github.com/opencontainers/image-spec/identity"
)

// ProbeImageRunner reads the version and ownership of the runner baked into
// an official runner image, by mounting a read-only VIEW of the image's
// rootfs and reading /home/runner/bin/Runner.Listener.deps.json. Nothing is
// written to the image; the view snapshot is removed before returning. The
// image is pulled and unpacked if needed, exactly as Create would.
//
// This is how boot-verify and the zero-work handler learn what the Linux
// jobs actually run (the embedded tarball is NOT it for official images).
func (r *Runtime) ProbeImageRunner(ctx context.Context, image string) (runnerversion.TreeInfo, error) {
	const ss = "overlayfs"
	img, err := r.client.GetImage(ctx, image)
	if err != nil {
		if err := r.PullImage(ctx, image); err != nil {
			return runnerversion.TreeInfo{}, fmt.Errorf("pulling %s: %w", image, err)
		}
		if img, err = r.client.GetImage(ctx, image); err != nil {
			return runnerversion.TreeInfo{}, fmt.Errorf("getting %s: %w", image, err)
		}
	}
	if unpacked, _ := img.IsUnpacked(ctx, ss); !unpacked {
		if err := img.Unpack(ctx, ss); err != nil {
			return runnerversion.TreeInfo{}, fmt.Errorf("unpacking %s: %w", image, err)
		}
	}
	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return runnerversion.TreeInfo{}, fmt.Errorf("rootfs of %s: %w", image, err)
	}
	parent := identity.ChainID(diffIDs).String()
	sn := r.client.SnapshotService(ss)
	key := fmt.Sprintf("ephemerd-runner-probe-%d", probeSeq.Add(1))
	mounts, err := sn.View(ctx, key, parent)
	if err != nil {
		return runnerversion.TreeInfo{}, fmt.Errorf("view snapshot of %s: %w", image, err)
	}
	defer func() {
		if rerr := sn.Remove(ctx, key); rerr != nil {
			r.cfg.Log.Warn("removing runner probe snapshot", "key", key, "error", rerr)
		}
	}()
	var info runnerversion.TreeInfo
	err = mount.WithTempMount(ctx, mounts, func(root string) error {
		var perr error
		info, perr = runnerversion.ProbeRunnerTree(filepath.Join(root, officialRunnerRoot))
		return perr
	})
	if err != nil {
		return info, fmt.Errorf("probing %s:%s: %w", image, officialRunnerRoot, err)
	}
	info.Dir = officialRunnerRoot
	return info, nil
}

// probeSeq makes view-snapshot keys unique across concurrent probes.
var probeSeq atomic.Int64
