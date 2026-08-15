// Package imagegc garbage-collects containerd image records under disk
// pressure.
//
// The model is kubelet's: **disk pressure is the trigger, least-recently-used
// is the order**. A high watermark starts a pass; the pass evicts LRU-first
// until a distinctly lower low watermark is reached, then stops. Age
// (`cache_max_age`) survives only as an optional, off-by-default backstop —
// evicting by age alone throws away a warm cache while the disk is half
// empty and forces re-downloads, which is the opposite of what we want on a
// node whose network budget we are also trying to shrink.
//
// Scope is every namespace that holds long-lived image records:
//
//   - "ephemerd", the main runtime namespace, whose images had NO garbage
//     collector at all before this package existed — every runner image and
//     every workflow `container:` image ever pulled was retained forever
//     along with its extracted overlay layers, which is what filled node
//     disks until QEMU froze;
//   - "ephemerd-dind-cache-*", the per-repo dind image caches, which already
//     had a correct LRU pruner (dind.CachePrune) that this package now backs.
//
// Per-job namespaces (ephemerd-dind-<runner>) are deliberately out of scope:
// they belong to a live job and are torn down by pkg/dind's own cleanup.
package imagegc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
)

// LastAccessedLabel records the most recent time an image record was pulled
// or used to start a container. It is the LRU key for every namespace this
// package collects, and is written in RFC3339 UTC.
//
// pkg/dind has stamped this on its cache records since the per-repo cache
// landed; pkg/runtime stamps it on the runtime namespace as of this package.
const LastAccessedLabel = "ephemerd.io/last-accessed"

// LastAccessed returns the time an image record was last used.
//
// Prefers LastAccessedLabel. Falls back to the record's UpdatedAt (then
// CreatedAt) so records that pre-date the label — every image on a node
// upgrading into this feature — sort by when containerd last touched them
// rather than being treated as never-used and evicted first.
func LastAccessed(img images.Image) time.Time {
	if ts := img.Labels[LastAccessedLabel]; ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t.UTC()
		}
	}
	if !img.UpdatedAt.IsZero() {
		return img.UpdatedAt.UTC()
	}
	return img.CreatedAt.UTC()
}

// Touch refreshes LastAccessedLabel on an image record so the LRU ordering
// reflects use, not just pull time. Best-effort: a missing image or a failed
// update is logged at debug and swallowed, because failing a job over a
// bookkeeping label would be absurd.
func Touch(ctx context.Context, c *client.Client, ns, name string, log *slog.Logger) {
	if c == nil || ns == "" || name == "" {
		return
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	img, err := c.ImageService().Get(nsCtx, name)
	if err != nil {
		if log != nil && !errdefs.IsNotFound(err) {
			log.Debug("imagegc: touch get", "namespace", ns, "image", name, "error", err)
		}
		return
	}
	if img.Labels == nil {
		img.Labels = map[string]string{}
	}
	img.Labels[LastAccessedLabel] = time.Now().UTC().Format(time.RFC3339)
	if _, err := c.ImageService().Update(nsCtx, img, "labels"); err != nil && log != nil {
		log.Debug("imagegc: touch update", "namespace", ns, "image", name, "error", err)
	}
}

// ResolveNamespaces returns the existing containerd namespaces matching
// either an exact name in exact or a prefix in prefixes, in the order
// containerd reports them. Names that do not exist are silently skipped.
func ResolveNamespaces(ctx context.Context, c *client.Client, exact, prefixes []string) ([]string, error) {
	all, err := c.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	exactSet := make(map[string]struct{}, len(exact))
	for _, e := range exact {
		exactSet[e] = struct{}{}
	}
	var out []string
	for _, ns := range all {
		if _, ok := exactSet[ns]; ok {
			out = append(out, ns)
			continue
		}
		for _, p := range prefixes {
			if p != "" && strings.HasPrefix(ns, p) {
				out = append(out, ns)
				break
			}
		}
	}
	return out, nil
}

// ListCandidates enumerates every image record in the given namespaces as an
// eviction candidate, resolving each one's last-accessed time and packed
// size.
//
// Size resolution is best-effort. It is a content-store metadata walk, not a
// disk traversal, but it can still fail — a partially-pulled image, or a
// manifest for a platform this client does not match. A candidate whose size
// cannot be determined gets SizeBytes 0, which PlanEviction reads as
// "unknown" rather than "free".
func ListCandidates(ctx context.Context, c *client.Client, nss []string, log *slog.Logger) ([]Candidate, error) {
	var out []Candidate
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		imgs, err := c.ImageService().List(nsCtx)
		if err != nil {
			if log != nil {
				log.Warn("imagegc: list images", "namespace", ns, "error", err)
			}
			continue
		}
		for _, img := range imgs {
			cand := Candidate{
				Namespace:    ns,
				Name:         img.Name,
				LastAccessed: LastAccessed(img),
			}
			if size, err := client.NewImage(c, img).Size(nsCtx); err == nil && size > 0 {
				cand.SizeBytes = size
			}
			out = append(out, cand)
		}
	}
	return out, nil
}

// RunningContainers returns the IDs of every container that currently
// exists and the set of image refs they reference, across every containerd
// namespace.
//
// Scope is deliberately all namespaces, not just the collected ones: content
// blobs are global, and a per-job dind namespace can hold a live sibling
// container built from the same ref that a cache namespace records. Deleting
// only the cache record would not free that content anyway, so protecting
// the ref everywhere costs nothing and removes a whole class of "we evicted
// an image a job was using" failure.
//
// The IDs are returned as well because some records are tied to a live job
// by NAME rather than by container reference — BuildKit export records are
// prefixed with the job's container ID (see Config.LiveJobPrefixes).
func RunningContainers(ctx context.Context, c *client.Client, log *slog.Logger) (ids, refs map[string]struct{}, err error) {
	all, err := c.NamespaceService().List(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list namespaces: %w", err)
	}
	ids = map[string]struct{}{}
	refs = map[string]struct{}{}
	for _, ns := range all {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		ctrs, lerr := c.ContainerService().List(nsCtx)
		if lerr != nil {
			if log != nil {
				log.Debug("imagegc: list containers", "namespace", ns, "error", lerr)
			}
			continue
		}
		for _, ctr := range ctrs {
			ids[ctr.ID] = struct{}{}
			if ctr.Image != "" {
				refs[ctr.Image] = struct{}{}
			}
		}
	}
	return ids, refs, nil
}

// RunningImageRefs returns just the image refs of existing containers.
// Thin wrapper over RunningContainers for callers that don't need the IDs.
func RunningImageRefs(ctx context.Context, c *client.Client, log *slog.Logger) (map[string]struct{}, error) {
	_, refs, err := RunningContainers(ctx, c, log)
	return refs, err
}

// ProtectedSet builds the never-evict set from a list of pinned refs plus
// the refs of every existing container. Both inputs are optional.
//
// Pinned refs are the node's configured runner images. Evicting one of those
// guarantees an immediate re-pull on the very next job — precisely the
// network thrash this collector exists to reduce — so they are vetoed
// regardless of how long they have sat idle.
func ProtectedSet(pinned []string, running map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(pinned)+len(running))
	for _, p := range pinned {
		if p != "" {
			out[p] = struct{}{}
		}
	}
	for r := range running {
		out[r] = struct{}{}
	}
	return out
}

// Delete removes one image record. Synchronous deletion makes containerd
// reclaim the now-unreferenced content and snapshots before returning, which
// is what lets the caller re-measure real free space between evictions
// instead of guessing from size estimates.
//
// A NotFound is success: something else already removed it.
func Delete(ctx context.Context, c *client.Client, cand Candidate, synchronous bool) error {
	nsCtx := namespaces.WithNamespace(ctx, cand.Namespace)
	var opts []images.DeleteOpt
	if synchronous {
		opts = append(opts, images.SynchronousDelete())
	}
	if err := c.ImageService().Delete(nsCtx, cand.Name, opts...); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("delete image %s in %s: %w", cand.Name, cand.Namespace, err)
	}
	return nil
}

// Evict deletes each candidate in order, logging and skipping failures so
// one stuck record cannot stall the whole pass. Returns the number deleted.
//
// stop, when non-nil, is consulted after every successful deletion;
// returning true ends the pass early. The pressure collector uses it to stop
// the moment a real disk re-measurement shows the low watermark reached.
func Evict(ctx context.Context, c *client.Client, cands []Candidate, synchronous bool, log *slog.Logger, stop func() bool) int {
	evicted := 0
	for _, cand := range cands {
		if err := ctx.Err(); err != nil {
			return evicted
		}
		if err := Delete(ctx, c, cand, synchronous); err != nil {
			if log != nil {
				log.Warn("imagegc: evict failed",
					"namespace", cand.Namespace, "image", cand.Name, "error", err)
			}
			continue
		}
		evicted++
		if log != nil {
			log.Info("imagegc: evicted image",
				"namespace", cand.Namespace, "image", cand.Name,
				"last_accessed", cand.LastAccessed, "size_bytes", cand.SizeBytes)
		}
		if stop != nil && stop() {
			return evicted
		}
	}
	return evicted
}
