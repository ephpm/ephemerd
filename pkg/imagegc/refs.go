package imagegc

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
)

// Reference-aware repair of the image ↔ snapshot relationship.
//
// containerd links an image record to its extracted layers with ONE edge:
// a `containerd.io/gc.ref.snapshot.<snapshotter>` label on the image's config
// blob, whose value is the chain ID of the top layer (see containerd
// core/unpack/unpacker.go and client/image.go; BuildKit's image exporter
// writes the same label in exporter/containerimage/export.go). That edge is
// what makes containerd's GC reference-counted: while the image record is
// alive, its layer chain is reachable and cannot be collected.
//
// The edge only protects against *collection*, though. Nothing protects
// against a snapshot being removed explicitly — `ctr snapshots rm`, a restored
// containerd data dir, a half-finished delete. When that happens the image
// record stays perfectly resolvable (manifest, config and layer blobs all
// present) while the rootfs it names is gone, and every consumer that trusts
// the record then fails on prepare. That is the "an image record resolves but
// its layer snapshot is gone" half of #149.
//
// So the collector gets a repair pass: follow each image record's edge, ask
// the snapshotter whether the target actually exists, and evict the records
// whose chain is broken. Evicting is the right move rather than leaving them:
// a broken record is worse than no record, because its presence is what stops
// the next job re-pulling a clean copy.

// SnapshotRefLabel is the content-store label that links a config blob to the
// chain ID of the snapshot chain unpacked from that image.
func SnapshotRefLabel(snapshotter string) string {
	return "containerd.io/gc.ref.snapshot." + snapshotter
}

// ImageChain is one image record and the snapshot chain it claims.
type ImageChain struct {
	Namespace string
	Name      string
	// SnapshotKey is the chain ID from the config blob's GC label. Empty
	// means the record names no snapshot at all — it was never unpacked,
	// which is normal for a pushed-but-not-run image and is NOT a broken
	// chain.
	SnapshotKey string
}

// Key is the namespace-qualified identifier, matching Candidate.Key.
func (i ImageChain) Key() string { return i.Namespace + "/" + i.Name }

// Candidate converts a chain back into an eviction candidate.
func (i ImageChain) Candidate() Candidate {
	return Candidate{Namespace: i.Namespace, Name: i.Name}
}

// PlanBrokenChains selects the image records whose snapshot chain is missing
// from the snapshotter. Pure — the whole "which records are unusable" rule in
// one testable function.
//
// A record is broken when it names a snapshot key that is not in existing.
// Records naming no key are skipped (never unpacked, nothing to break), and
// so are protected records: a pinned runner image or an image a live
// container is using must not be evicted even when its chain looks broken,
// because evicting it cannot help — the container already holds the rootfs —
// and re-pulling a multi-gigabyte runner image mid-job is strictly worse.
// Those get reported through brokenProtected so the caller can log them
// instead of silently doing nothing.
//
// Output is sorted by Key so repeated passes over the same store produce the
// same plan.
func PlanBrokenChains(chains []ImageChain, existing, protected map[string]struct{}) (broken []ImageChain, brokenProtected []ImageChain) {
	for _, ch := range chains {
		if ch.SnapshotKey == "" {
			continue
		}
		if _, ok := existing[ch.SnapshotKey]; ok {
			continue
		}
		if _, ok := protected[ch.Name]; ok {
			brokenProtected = append(brokenProtected, ch)
			continue
		}
		broken = append(broken, ch)
	}
	sortChains(broken)
	sortChains(brokenProtected)
	return broken, brokenProtected
}

// ChainsReferencing returns the chains whose SnapshotKey equals key. Pure.
// Used by the auto-heal path, which knows exactly which snapshot went missing
// and wants the image records that name it.
func ChainsReferencing(chains []ImageChain, key string) []ImageChain {
	if key == "" {
		return nil
	}
	var out []ImageChain
	for _, ch := range chains {
		if ch.SnapshotKey == key {
			out = append(out, ch)
		}
	}
	sortChains(out)
	return out
}

func sortChains(c []ImageChain) {
	sort.Slice(c, func(i, j int) bool { return c[i].Key() < c[j].Key() })
}

// ListImageChains resolves every image record in nss to the snapshot chain it
// names, by reading the GC label off each record's config blob.
//
// Best-effort per record: an image whose manifest is missing, whose platform
// does not match this node, or whose config blob has been collected simply
// yields no chain rather than failing the pass. Those records are a different
// (content-level) kind of broken and are out of scope here.
func ListImageChains(ctx context.Context, c *client.Client, nss []string, snapshotter string, log *slog.Logger) ([]ImageChain, error) {
	if c == nil || snapshotter == "" {
		return nil, nil
	}
	label := SnapshotRefLabel(snapshotter)
	cs := c.ContentStore()

	var out []ImageChain
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		imgs, err := c.ImageService().List(nsCtx)
		if err != nil {
			if log != nil {
				log.Warn("imagegc: list images for chain scan", "namespace", ns, "error", err)
			}
			continue
		}
		for _, img := range imgs {
			key, err := imageSnapshotKey(nsCtx, cs, img, label)
			if err != nil {
				if log != nil {
					log.Debug("imagegc: resolve snapshot chain",
						"namespace", ns, "image", img.Name, "error", err)
				}
			}
			out = append(out, ImageChain{Namespace: ns, Name: img.Name, SnapshotKey: key})
		}
	}
	return out, nil
}

// imageSnapshotKey reads the chain ID an image record names, or "" if it
// names none.
func imageSnapshotKey(nsCtx context.Context, cs content.Store, img images.Image, label string) (string, error) {
	cfg, err := images.Config(nsCtx, cs, img.Target, platforms.Default())
	if err != nil {
		return "", fmt.Errorf("config descriptor: %w", err)
	}
	info, err := cs.Info(nsCtx, cfg.Digest)
	if err != nil {
		return "", fmt.Errorf("config blob info: %w", err)
	}
	return info.Labels[label], nil
}

// ExistingSnapshots returns the subset of keys the snapshotter actually has,
// per namespace. Snapshots are namespaced, so a key is checked in the
// namespace of the record that named it.
//
// A Stat error that is not NotFound is treated as "exists": under uncertainty
// the safe answer is to leave the record alone, because the cost of a wrong
// "missing" verdict is evicting a healthy image.
func ExistingSnapshots(ctx context.Context, c *client.Client, snapshotter string, chains []ImageChain, log *slog.Logger) map[string]struct{} {
	out := map[string]struct{}{}
	if c == nil || snapshotter == "" {
		return out
	}
	sn := c.SnapshotService(snapshotter)
	checked := map[string]struct{}{}
	for _, ch := range chains {
		if ch.SnapshotKey == "" {
			continue
		}
		probe := ch.Namespace + "\x00" + ch.SnapshotKey
		if _, done := checked[probe]; done {
			continue
		}
		checked[probe] = struct{}{}

		nsCtx := namespaces.WithNamespace(ctx, ch.Namespace)
		if _, err := sn.Stat(nsCtx, ch.SnapshotKey); err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			if log != nil {
				log.Debug("imagegc: snapshot stat failed; assuming present",
					"namespace", ch.Namespace, "snapshot", ch.SnapshotKey, "error", err)
			}
		}
		out[ch.SnapshotKey] = struct{}{}
	}
	return out
}

// RepairBrokenChains evicts every image record in nss whose snapshot chain is
// missing, and returns how many it removed.
//
// This is ask (1) of #149's second half — "if a chain is already broken, evict
// the image record too" — and it is what makes the failure self-limiting from
// the collector's side: a node that gets its snapshots removed out of band
// heals on the next sweep instead of failing every job until someone logs in.
func RepairBrokenChains(ctx context.Context, c *client.Client, nss []string, snapshotter string, protected map[string]struct{}, log *slog.Logger) (int, error) {
	chains, err := ListImageChains(ctx, c, nss, snapshotter, log)
	if err != nil {
		return 0, err
	}
	if len(chains) == 0 {
		return 0, nil
	}
	existing := ExistingSnapshots(ctx, c, snapshotter, chains, log)
	broken, brokenProtected := PlanBrokenChains(chains, existing, protected)

	for _, ch := range brokenProtected {
		if log != nil {
			log.Error("imagegc: protected image has a broken layer chain and cannot be repaired automatically; remove it by hand once no job needs it",
				"namespace", ch.Namespace, "image", ch.Name, "snapshot", ch.SnapshotKey)
		}
	}
	if len(broken) == 0 {
		return 0, nil
	}

	repaired := 0
	for _, ch := range broken {
		if err := Delete(ctx, c, ch.Candidate(), true); err != nil {
			if log != nil {
				log.Warn("imagegc: evicting broken-chain image failed",
					"namespace", ch.Namespace, "image", ch.Name, "error", err)
			}
			continue
		}
		repaired++
		if log != nil {
			log.Warn("imagegc: evicted image whose layer snapshot is gone; next pull will be clean",
				"namespace", ch.Namespace, "image", ch.Name, "snapshot", ch.SnapshotKey)
		}
	}
	return repaired, nil
}

// EvictReferencing removes the image records in nss that name snapshot key.
// The auto-heal path calls this when a build has already told us exactly which
// snapshot is missing, so there is no need to scan the whole store.
//
// protected is honoured for the same reason as in PlanBrokenChains.
func EvictReferencing(ctx context.Context, c *client.Client, nss []string, snapshotter, key string, protected map[string]struct{}, log *slog.Logger) (int, error) {
	if key == "" {
		return 0, nil
	}
	chains, err := ListImageChains(ctx, c, nss, snapshotter, log)
	if err != nil {
		return 0, err
	}
	evicted := 0
	for _, ch := range ChainsReferencing(chains, key) {
		if _, ok := protected[ch.Name]; ok {
			if log != nil {
				log.Error("imagegc: protected image references the missing snapshot; not evicting",
					"namespace", ch.Namespace, "image", ch.Name, "snapshot", key)
			}
			continue
		}
		if err := Delete(ctx, c, ch.Candidate(), true); err != nil {
			if log != nil {
				log.Warn("imagegc: evicting image that references a missing snapshot failed",
					"namespace", ch.Namespace, "image", ch.Name, "error", err)
			}
			continue
		}
		evicted++
		if log != nil {
			log.Warn("imagegc: evicted image referencing a missing snapshot",
				"namespace", ch.Namespace, "image", ch.Name, "snapshot", key)
		}
	}
	return evicted, nil
}
