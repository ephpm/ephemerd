package dind

import (
	"context"
	"fmt"
	"log/slog"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
)

// DindNamespacePrefix is the prefix every per-job containerd namespace the
// dind subsystem creates. Each running ephpm-style job has its own namespace
// (e.g. "ephemerd-dind-ephemerd-github-ephpm-fast_shannon") so containers,
// images, and leases from one job can't pin disk against another's.
const DindNamespacePrefix = "ephemerd-dind-"

// CleanupJobNamespace removes everything inside a per-job dind namespace and
// then the namespace metadata bucket itself. Safe to call multiple times and
// safe to call on a namespace that contains stragglers from prior crashes —
// each step logs and continues on error rather than bailing partway through.
//
// Order matters:
//  1. Containers (with their tasks + snapshots) — releases the overlayfs
//     upperdirs that hold container rootfs writes.
//  2. Images — drops the gc.ref labels that pin manifest+config+layer blobs.
//  3. Leases — releases any explicit content holds (buildkit etc. take these
//     during pulls/builds).
//  4. Snapshots — orphan snapshots from layers that were unpinned in step 2
//     don't get reclaimed by containerd's async GC fast enough; the
//     NamespaceService.Delete in step 6 would fail with FailedPrecondition
//     until those snapshots are gone. Walk the snapshotter and remove them
//     explicitly.
//  5. Content blobs — same story as snapshots; the async GC won't have
//     swept by the time we try to delete the namespace.
//  6. NamespaceService.Delete — drops the metadata bucket. Will only succeed
//     if 1-5 left the namespace truly empty; on failure we log and leave
//     the bucket so a subsequent boot's CleanupStaleDindNamespaces can retry.
func CleanupJobNamespace(ctx context.Context, c *client.Client, ns string, log *slog.Logger) {
	if c == nil || ns == "" {
		return
	}
	log = log.With("namespace", ns)
	nsCtx := namespaces.WithNamespace(ctx, ns)

	// 1. Containers + tasks + snapshots-attached-to-containers.
	containers, err := c.Containers(nsCtx)
	if err != nil && !errdefs.IsNotFound(err) {
		log.Warn("dind cleanup: list containers", "error", err)
	}
	for _, cnt := range containers {
		id := cnt.ID()
		if task, terr := cnt.Task(nsCtx, nil); terr == nil && task != nil {
			if status, serr := task.Status(nsCtx); serr == nil && status.Status == client.Running {
				if kerr := task.Kill(nsCtx, 9); kerr != nil {
					log.Debug("dind cleanup: task kill", "container", id, "error", kerr)
				}
				if exitCh, werr := task.Wait(nsCtx); werr == nil {
					<-exitCh
				}
			}
			if _, derr := task.Delete(nsCtx, client.WithProcessKill); derr != nil {
				log.Debug("dind cleanup: task delete", "container", id, "error", derr)
			}
		}
		if derr := cnt.Delete(nsCtx, client.WithSnapshotCleanup); derr != nil {
			log.Warn("dind cleanup: container delete", "container", id, "error", derr)
		}
	}

	// 2. Images. Each Image record holds gc.ref labels to its manifest +
	//    config + layer blobs; deleting the image releases those refs.
	if imgs, ierr := c.ImageService().List(nsCtx); ierr != nil && !errdefs.IsNotFound(ierr) {
		log.Warn("dind cleanup: list images", "error", ierr)
	} else {
		for _, img := range imgs {
			if derr := c.ImageService().Delete(nsCtx, img.Name); derr != nil && !errdefs.IsNotFound(derr) {
				log.Warn("dind cleanup: image delete", "image", img.Name, "error", derr)
			}
		}
	}

	// 3. Leases.
	leasesSvc := c.LeasesService()
	if ls, lerr := leasesSvc.List(nsCtx); lerr != nil && !errdefs.IsNotFound(lerr) {
		log.Warn("dind cleanup: list leases", "error", lerr)
	} else {
		for _, l := range ls {
			if derr := leasesSvc.Delete(nsCtx, l); derr != nil && !errdefs.IsNotFound(derr) {
				log.Warn("dind cleanup: lease delete", "lease", l.ID, "error", derr)
			}
		}
	}

	// 4. Snapshots. Containerd's namespace-Delete requires the snapshotter
	//    to also report empty, but async GC won't have swept the snapshots
	//    that 1-3 unpinned. Image layer snapshots form a parent-child tree
	//    (each layer is a child of the one below) and containerd refuses to
	//    delete a snapshot that still has children, so we have to remove
	//    leaves-first. Iterate until either the snapshotter is empty or a
	//    pass makes no progress (something else is pinning a node).
	for _, snName := range snapshotterNames() {
		snSvc := c.SnapshotService(snName)
		if snSvc == nil {
			continue
		}
		// Bound the loop at len(keys) passes — each pass that makes
		// progress removes at least one leaf, so it can't take more
		// than O(len) passes to drain a tree.
		for pass := 0; ; pass++ {
			var keys []string
			walkErr := snSvc.Walk(nsCtx, func(_ context.Context, info snapshots.Info) error {
				keys = append(keys, info.Name)
				return nil
			})
			if walkErr != nil && !errdefs.IsNotFound(walkErr) {
				log.Warn("dind cleanup: walk snapshots", "snapshotter", snName, "error", walkErr)
				break
			}
			if len(keys) == 0 {
				break
			}
			if pass > len(keys)+1 {
				// Defensive: shouldn't happen for valid trees, but guard
				// against a pathological case that would loop forever.
				log.Warn("dind cleanup: snapshot removal not converging",
					"snapshotter", snName, "remaining", len(keys))
				break
			}
			progress := false
			for _, key := range keys {
				if derr := snSvc.Remove(nsCtx, key); derr != nil {
					if errdefs.IsNotFound(derr) {
						continue
					}
					if errdefs.IsFailedPrecondition(derr) {
						// Parent of an as-yet-unremoved child. Skip;
						// next pass will catch it once the leaf goes.
						continue
					}
					log.Warn("dind cleanup: snapshot remove",
						"snapshotter", snName, "key", key, "error", derr)
					continue
				}
				progress = true
			}
			if !progress {
				// Log per-snapshot detail so we can tell whether the stuck
				// snapshot is a kindest/node tmpfs view, a buildkit-managed
				// snapshot, or something else. Reproduces only the stuck
				// ones (the leaves we already removed are gone).
				for _, key := range keys {
					info, statErr := snSvc.Stat(nsCtx, key)
					if statErr != nil {
						log.Warn("dind cleanup: stuck snapshot stat failed",
							"snapshotter", snName, "key", key, "error", statErr)
						continue
					}
					log.Warn("dind cleanup: stuck snapshot",
						"snapshotter", snName,
						"key", info.Name,
						"parent", info.Parent,
						"kind", info.Kind.String(),
						"labels", info.Labels)
				}
				break
			}
		}
	}

	// 4b. Best-effort retry of snapshot removal after a short delay — gives
	// containerd's async GC a moment to release anything we couldn't
	// directly remove (e.g. a recently-unmounted view that hasn't propagated
	// yet). Bounded so we don't sit here forever on a genuinely stuck node.
	for attempt := 0; attempt < 3; attempt++ {
		stillThere := false
		for _, snName := range snapshotterNames() {
			snSvc := c.SnapshotService(snName)
			if snSvc == nil {
				continue
			}
			if walkErr := snSvc.Walk(nsCtx, func(_ context.Context, info snapshots.Info) error {
				stillThere = true
				if derr := snSvc.Remove(nsCtx, info.Name); derr != nil && !errdefs.IsNotFound(derr) && !errdefs.IsFailedPrecondition(derr) {
					log.Debug("dind cleanup: retry snapshot remove",
						"snapshotter", snName, "key", info.Name, "error", derr)
				}
				return nil
			}); walkErr != nil {
				log.Debug("dind cleanup: retry walk", "snapshotter", snName, "error", walkErr)
			}
		}
		if !stillThere {
			break
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}

	// 5. Content blobs. Same story — gc.ref labels were dropped in step 2,
	//    but the content store's actual blob entries linger until the next
	//    GC pass. Walk and delete.
	cs := c.ContentStore()
	if cs != nil {
		var digests []string
		walkErr := cs.Walk(nsCtx, func(info content.Info) error {
			digests = append(digests, info.Digest.String())
			return nil
		})
		if walkErr != nil && !errdefs.IsNotFound(walkErr) {
			log.Warn("dind cleanup: walk content", "error", walkErr)
		}
		for _, d := range digests {
			dgst, perr := digest.Parse(d)
			if perr != nil {
				log.Debug("dind cleanup: parse content digest", "digest", d, "error", perr)
				continue
			}
			if derr := cs.Delete(nsCtx, dgst); derr != nil && !errdefs.IsNotFound(derr) {
				// Content is reference-counted; if another namespace still
				// pins it (shared content with shareable label set), this
				// fails. Log debug — that's expected, not a leak in OUR ns.
				log.Debug("dind cleanup: content delete", "digest", d, "error", derr)
			}
		}
	}

	// 6. Finally drop the namespace metadata bucket. If the bucket was never
	//    materialized (short job that didn't touch docker), Delete returns
	//    NotFound — that's fine, downgrade to debug.
	if derr := c.NamespaceService().Delete(nsCtx, ns); derr != nil {
		if errdefs.IsNotFound(derr) {
			log.Debug("dind cleanup: namespace never materialized")
		} else {
			log.Warn("dind cleanup: namespace delete", "error", derr)
		}
	} else {
		log.Info("dind cleanup: namespace removed")
	}
}

// CleanupStaleDindNamespaces enumerates every namespace matching
// DindNamespacePrefix and runs CleanupJobNamespace on each. Intended to be
// called once at ephemerd worker-mode startup to clean up after crashed or
// killed jobs from a previous boot — the same Server.Stop path would have
// done this on a graceful shutdown but a runner timeout / SIGKILL skips it.
func CleanupStaleDindNamespaces(ctx context.Context, c *client.Client, log *slog.Logger) {
	if c == nil {
		return
	}
	all, err := c.NamespaceService().List(ctx)
	if err != nil {
		log.Warn("dind cleanup: list namespaces", "error", err)
		return
	}
	count := 0
	for _, ns := range all {
		if !strings.HasPrefix(ns, DindNamespacePrefix) {
			continue
		}
		// Don't touch the long-lived per-repo image caches; only per-job
		// namespaces should be swept here. Cache pruning is a separate
		// concern handled by CachePrune.
		if strings.HasPrefix(ns, DindCacheNamespacePrefix) {
			continue
		}
		count++
		CleanupJobNamespace(ctx, c, ns, log)
	}
	if count > 0 {
		log.Info("dind cleanup: stale namespaces processed", "count", count)
	}
}

// snapshotterNames returns the snapshotter names dind/containerd uses on this
// platform. On Linux that's "overlayfs"; on Windows we use the "windows"
// snapshotter. We try every plausible name and skip ones that don't exist
// so the cleanup is robust against future snapshotter changes.
func snapshotterNames() []string {
	switch goruntime.GOOS {
	case "windows":
		return []string{"windows", "windows-lcow"}
	default:
		return []string{"overlayfs", "native"}
	}
}

// dindNamespaceFromJobID returns the containerd namespace name a dind Server
// uses for a given jobID. Exposed (lowercased) so callers that have the
// JobID — not a *Server — can construct the namespace name consistently.
//
//nolint:unused // exposed for symmetry with Server.jobNamespace; future
// boot-time selective cleanup may use it.
func dindNamespaceFromJobID(jobID string) string {
	return fmt.Sprintf("%s%s", DindNamespacePrefix, jobID)
}
