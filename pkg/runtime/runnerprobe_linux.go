//go:build linux

package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/ephpm/ephemerd/pkg/runnerversion"
	"github.com/opencontainers/image-spec/identity"
)

// probeViewPrefix names every view snapshot a probe creates, so a boot
// sweep can recognise (and only recognise) leaked ones.
const probeViewPrefix = "ephemerd-runner-probe-"

// probeLeaseTTL bounds how long a probe's lease pins the view and the
// image content it sits on. A probe takes well under a second once the
// image is unpacked; the TTL is the crash backstop, not the budget.
const probeLeaseTTL = 10 * time.Minute

// ProbeImageRunner reads the version and ownership of the runner baked into
// an official runner image, by mounting a read-only VIEW of the image's
// rootfs and reading /home/runner/bin/Runner.Listener.deps.json. Nothing is
// written to the image; the view snapshot is removed before returning. The
// image is pulled and unpacked if needed, exactly as Create would.
//
// This is how boot-verify and the zero-work handler learn what the Linux
// jobs actually run (the embedded tarball is NOT it for official images).
//
// The view is created under a short-lived containerd lease: without one,
// a GC pass between View and the temp mount could delete the snapshot (or
// the image layers it references) under a probe that happens to coincide
// with an image prune, and the probe would fail or read a half-removed
// tree. The lease is deleted on the way out.
func (r *Runtime) ProbeImageRunner(ctx context.Context, image string) (runnerversion.TreeInfo, error) {
	const ss = "overlayfs"
	ctx = namespaces.WithNamespace(ctx, namespace)
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

	// Hold a lease over the view for the probe's lifetime. A lease that
	// cannot be created degrades to the unleased probe (strictly better
	// than no probe) rather than failing the self-heal outright.
	releaseLease := func() {}
	if lm := r.client.LeasesService(); lm != nil {
		if l, lerr := lm.Create(ctx, leases.WithRandomID(), leases.WithExpiration(probeLeaseTTL)); lerr == nil {
			ctx = leases.WithLease(ctx, l.ID)
			releaseLease = func() {
				if derr := lm.Delete(ctx, l, leases.SynchronousDelete); derr != nil && !errdefs.IsNotFound(derr) {
					r.cfg.Log.Warn("releasing runner probe lease", "lease", l.ID, "error", derr)
				}
			}
		} else {
			r.cfg.Log.Debug("runner probe: lease unavailable, probing unleased", "error", lerr)
		}
	}
	defer releaseLease()

	sn := r.client.SnapshotService(ss)
	// The key carries the boot time as well as a sequence number: a view
	// left behind by a crash mid-probe must not collide with the first
	// probe of the next boot (sequence numbers restart at 1).
	key := fmt.Sprintf("%s%d-%d", probeViewPrefix, probeEpoch, probeSeq.Add(1))
	mounts, err := sn.View(ctx, key, parent)
	if err != nil {
		return runnerversion.TreeInfo{}, fmt.Errorf("view snapshot of %s: %w", image, err)
	}
	defer func() {
		if rerr := sn.Remove(ctx, key); rerr != nil && !errdefs.IsNotFound(rerr) {
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

// SweepRunnerProbeViews removes view snapshots a previous daemon left
// behind (a crash between View and Remove). Called once at boot before the
// first probe. Returns how many were removed; errors on individual views
// are logged, not returned — a stuck view must not block boot.
func (r *Runtime) SweepRunnerProbeViews(ctx context.Context) int {
	ctx = namespaces.WithNamespace(ctx, namespace)
	sn := r.client.SnapshotService("overlayfs")
	var stale []string
	err := sn.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		if strings.HasPrefix(info.Name, probeViewPrefix) && info.Kind == snapshots.KindView {
			stale = append(stale, info.Name)
		}
		return nil
	})
	if err != nil {
		r.cfg.Log.Warn("sweeping runner probe views: walk failed", "error", err)
		return 0
	}
	removed := 0
	for _, key := range stale {
		if rerr := sn.Remove(ctx, key); rerr != nil && !errdefs.IsNotFound(rerr) {
			r.cfg.Log.Warn("sweeping runner probe views: remove failed", "key", key, "error", rerr)
			continue
		}
		removed++
	}
	if removed > 0 {
		r.cfg.Log.Info("removed leaked runner probe views from a previous boot", "count", removed)
	}
	return removed
}

// probeSeq makes view-snapshot keys unique across concurrent probes;
// probeEpoch makes them unique across daemon restarts.
var (
	probeSeq   atomic.Int64
	probeEpoch = time.Now().UnixNano()
)
