package dind

import (
	"context"
	"log/slog"

	"github.com/containerd/containerd/v2/client"
	"github.com/ephpm/ephemerd/pkg/buildkit"
	"github.com/ephpm/ephemerd/pkg/imagegc"
)

// Auto-repair of the shared build store, ask (2) of #149.
//
// The failure this exists for is not that a build broke — builds break all the
// time. It is that a build broke in a way NO JOB CAN FIX. By design (#137) a
// job cannot evict an image from the shared namespace, `docker builder prune`
// only reaches the job's own namespaced view, and removing the buildx state
// volume does nothing to a store that lives on the host. So one dangling
// snapshot in the shared BuildKit store failed every subsequent build on the
// fleet's only linux-amd64 runner, six release attempts in a row, until a
// human logged into the node.
//
// The point of healing here is therefore not elegance, it is making the blast
// radius finite: whatever poisons the shared store — an operator's manual
// reclaim, a restored data dir, a crash mid-delete — costs one build instead
// of every build.

// healAndRetryBuild inspects a failed solve. When the failure is the
// dangling-snapshot signature it repairs the shared store and reports whether
// the caller should retry the build once.
//
// Repairing before retrying rather than merely marking the store dirty for the
// next job is deliberate: the job that paid for the discovery is the one that
// gets to succeed. A job that hits this and then passes is indistinguishable
// from a slightly slow build; a job that hits it and fails is a red release
// pipeline someone has to triage.
func (s *Server) healAndRetryBuild(ctx context.Context, bk *buildkit.Server, solveErr error) (retry bool) {
	d, ok := buildkit.DanglingSnapshotFromError(solveErr)
	if !ok {
		return false
	}

	action := bk.Healer().Next(d)
	log := s.log.With("component", "build-heal", "snapshot", d.ID, "kind", d.Kind.String(), "action", action.String())

	if action == buildkit.HealGiveUp {
		log.Error("shared build store still names a snapshot containerd does not have, after both a prune and a metadata rebuild — not retrying; inspect the node")
		return false
	}

	log.Warn("build failed on a snapshot the shared store references but containerd does not have; repairing the shared store")

	report := buildkit.HealReport{Snapshot: d, Action: action}

	// Reference-aware half: an image record that names the missing chain is
	// worse than no record, because its presence is what stops the next pull
	// fetching a clean copy. Only chain IDs can be named by an image record;
	// a BuildKit cache-record ID never is.
	if d.Kind == buildkit.SnapshotChainID && s.client != nil {
		n, err := imagegc.EvictReferencing(ctx, s.client,
			s.healNamespaces(bk), bk.Snapshotter(), d.ID, s.healProtected(ctx), log)
		if err != nil {
			log.Warn("evicting image records that name the missing snapshot failed", "error", err)
		}
		report.ImagesEvicted = n
	}

	switch action {
	case buildkit.HealPrune:
		released, err := bk.PruneAll(ctx)
		if err != nil {
			// A prune that cannot even enumerate the store is itself
			// evidence the metadata is inconsistent. Escalate now rather
			// than burning another build to learn the same thing.
			log.Warn("build cache prune failed; escalating to a metadata rebuild", "error", err)
			if rerr := bk.Rebuild(ctx); rerr != nil {
				log.Error("rebuilding the buildkit metadata store failed", "error", rerr)
				return false
			}
			report.Rebuilt = true
		}
		report.BytesReleased = released
	case buildkit.HealRebuild:
		if err := bk.Rebuild(ctx); err != nil {
			log.Error("rebuilding the buildkit metadata store failed", "error", err)
			return false
		}
		report.Rebuilt = true
	}

	log.Warn("shared build store repaired; retrying the build once", "report", report.String())
	return true
}

// healNamespaces lists the containerd namespaces a repair may evict image
// records from. Only the shared BuildKit namespace and this job's own
// namespaces qualify — never the runtime namespace, whose records are the
// node's runner images.
// Empty names are dropped: "" is not "no namespace" to containerd's client,
// it is an invalid one, and a List against it fails the whole repair.
func (s *Server) healNamespaces(bk *buildkit.Server) []string {
	var nss []string
	for _, ns := range []string{bk.ContainerdNamespace(), s.jobNamespace, s.cacheNamespace} {
		if ns != "" {
			nss = append(nss, ns)
		}
	}
	return nss
}

// healProtected is the never-evict set for a repair: every image any live
// container references. A repair runs while other jobs are building on the
// same node, and evicting an image out from under one of them would turn one
// broken build into two.
func (s *Server) healProtected(ctx context.Context) map[string]struct{} {
	running, err := imagegc.RunningImageRefs(ctx, s.client, s.log)
	if err != nil {
		// Without the live set we cannot tell which records are in use.
		// Protect everything by name rather than risk evicting a running
		// job's image; the prune half of the repair still runs.
		s.log.Warn("build heal: listing running containers failed; skipping image eviction", "error", err)
		return nil
	}
	return imagegc.ProtectedSet(nil, running)
}

// sweepBrokenImageChains evicts image records anywhere on the node whose
// layer snapshot has gone missing. Runs on the node disk sweeper's timer so a
// node repairs itself in the background, without waiting for a job to trip
// over the broken record first.
//
// Deliberately independent of whether image GC is enabled: this is a
// correctness repair, not a capacity policy, and a node with GC turned off is
// exactly the node most likely to be carrying a broken store.
func SweepBrokenImageChains(ctx context.Context, c *client.Client, nss []string, snapshotter string, pinned []string, log *slog.Logger) {
	if c == nil || snapshotter == "" {
		return
	}
	running, err := imagegc.RunningImageRefs(ctx, c, log)
	if err != nil {
		log.Warn("broken-chain sweep: listing containers failed", "error", err)
		return
	}
	n, err := imagegc.RepairBrokenChains(ctx, c, nss, snapshotter, imagegc.ProtectedSet(pinned, running), log)
	if err != nil {
		log.Warn("broken-chain sweep failed", "error", err)
		return
	}
	if n > 0 {
		log.Warn("broken-chain sweep evicted image records whose layers were gone", "count", n)
	}
}
