package dind

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/ephpm/ephemerd/pkg/imagegc"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// DindCacheNamespacePrefix prefixes every per-repo image cache namespace.
//
// Full namespace name format:
//
//	ephemerd-dind-cache-<provider>-<sanitized-repo>
//
// Examples:
//
//	ephemerd-dind-cache-github-ephpm_ephpm
//	ephemerd-dind-cache-gitea-ephpm_ephpm        (distinct from the github one)
//	ephemerd-dind-cache-gitlab-acme_platform_api (nested GitLab groups OK)
//
// Provider + repo together form the privacy boundary: two different forges
// with same-named repos do NOT share a cache, and two different orgs on the
// same forge get separate caches keyed by the full `owner/repo` path.
const DindCacheNamespacePrefix = "ephemerd-dind-cache-"

// LastAccessedLabel records the most recent time an Image record in a cache
// namespace was touched (pull or container-create). The pruner uses this
// for LRU eviction. RFC3339-formatted, UTC.
//
// Aliased to pkg/imagegc, which owns the label now that the same LRU key is
// stamped on the main "ephemerd" runtime namespace too.
const LastAccessedLabel = imagegc.LastAccessedLabel

// CacheNamespace returns the containerd namespace name used to cache image
// metadata for a given (provider, repo) pair. Both inputs are sanitized so
// the result is always a valid containerd namespace identifier (regex:
// ^[A-Za-z0-9]+(?:[._-]+[A-Za-z0-9]+)*$).
//
// Provider should be the value from providers.Provider.Name() (e.g.
// "github", "gitea"). Repo is the forge-native repo path (e.g.
// "owner/repo" on GitHub or "group/subgroup/project" on GitLab); path
// separators are mapped to underscores so the namespace identifier stays
// valid. Empty provider or repo returns "" — callers should treat that as
// "caching disabled for this job".
func CacheNamespace(provider, repo string) string {
	provider = sanitizeForNamespace(provider)
	repo = sanitizeForNamespace(repo)
	if provider == "" || repo == "" {
		return ""
	}
	return DindCacheNamespacePrefix + provider + "-" + repo
}

// sanitizeForNamespace replaces every character that's not allowed in a
// containerd namespace identifier with an underscore, then collapses runs
// of underscores and trims leading/trailing ones. Containerd allows
// alphanumerics with `_`, `-`, `.` between them.
func sanitizeForNamespace(s string) string {
	if s == "" {
		return ""
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '.':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	// Collapse repeated separators and trim leading/trailing ones so
	// containerd's regex (which forbids consecutive separators outside
	// alphanumeric runs) accepts the result.
	collapsed := make([]byte, 0, len(out))
	var prev byte
	for _, c := range out {
		if (c == '_' || c == '-' || c == '.') && (prev == '_' || prev == '-' || prev == '.') {
			continue
		}
		collapsed = append(collapsed, c)
		prev = c
	}
	return strings.Trim(string(collapsed), "_-.")
}

// mirrorLeaseTTL bounds how long the temporary lease taken during a mirror
// can survive if the process dies mid-copy. containerd expires the lease on
// its own after this, so a crashed mirror can't pin blobs forever.
const mirrorLeaseTTL = time.Hour

// MirrorImageToCache copies an image from the per-job namespace into the
// per-repo cache namespace (creating it if needed): every content reference
// in the image's DAG first, then the Image record itself, with the
// LastAccessedLabel refreshed.
//
// Copying the *content references* — not just the Image record — is what
// makes the cache a cache. containerd's GC works on (namespace, digest)
// nodes, and `metadata.contentStore.garbageCollect` deletes a backing blob
// as soon as no namespace holds a blob bucket for it. An Image record's
// target and gc.ref labels only nominate nodes; if the cache namespace has
// no bucket for a digest there is nothing for the mark phase to keep, so
// dropping the per-job namespace made the whole image collectable and the
// next job re-pulled it over the network.
//
// The copy costs no bytes and no network. containerd's default content
// sharing policy is "shared", so opening a writer in the cache namespace
// for a digest that is already in the backing store short-circuits
// (metadata.contentStore.Writer's `cs.shared` branch) and Commit only
// creates the bucket. Under an "isolated" policy there is no short-circuit,
// so we stream the bytes from the job namespace — still local disk, never
// the registry.
//
// Once the buckets exist, the next job's pull into a fresh namespace hits
// the same short-circuit in remotes.fetch (`ws.Offset == desc.Size`) and
// downloads nothing.
//
// Returns nil if the cache namespace name is empty (no provider/repo set).
func MirrorImageToCache(ctx context.Context, c *client.Client, jobNS, cacheNS, imageName string, log *slog.Logger) error {
	if c == nil || cacheNS == "" || imageName == "" {
		return nil
	}
	jobCtx := namespaces.WithNamespace(ctx, jobNS)
	jobImg, err := c.ImageService().Get(jobCtx, imageName)
	if err != nil {
		return fmt.Errorf("get image %q in %s: %w", imageName, jobNS, err)
	}

	cacheCtx := namespaces.WithNamespace(ctx, cacheNS)

	// Content first, under a lease. Between the first blob bucket landing
	// in the cache namespace and the Image record that will reference it
	// being created, nothing else roots those buckets — a GC pass in that
	// window would sweep them straight back out. Every bucket created
	// under a leased context is attached to the lease
	// (metadata.contentStore.Writer -> addContentLease), so the lease
	// covers exactly the gap.
	release, leasedCtx := beginMirrorLease(c, cacheCtx, log)
	defer release()

	copied, err := mirrorContentRefs(jobCtx, leasedCtx, c.ContentStore(), jobImg.Target, log)
	if err != nil {
		return fmt.Errorf("mirror content for %q into %s: %w", imageName, cacheNS, err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if jobImg.Labels == nil {
		jobImg.Labels = map[string]string{}
	}
	jobImg.Labels[LastAccessedLabel] = now

	// Try Create first. If the image already exists in the cache (re-pull
	// of an already-cached tag), Create returns AlreadyExists and we
	// Update the existing record instead so the LastAccessedLabel refresh
	// takes effect.
	if _, cerr := c.ImageService().Create(cacheCtx, jobImg); cerr != nil {
		if !errdefs.IsAlreadyExists(cerr) {
			return fmt.Errorf("create image %q in %s: %w", imageName, cacheNS, cerr)
		}
		if _, uerr := c.ImageService().Update(cacheCtx, jobImg, "labels", "target"); uerr != nil {
			return fmt.Errorf("update image %q in %s: %w", imageName, cacheNS, uerr)
		}
	}
	log.Debug("dind cache: mirrored image",
		"image", imageName, "cache", cacheNS, "content_refs", copied)
	return nil
}

// beginMirrorLease creates a short-lived lease in the cache namespace and
// returns a context carrying it plus a release func. If the lease can't be
// created the mirror still proceeds — an unleased copy is racy against a
// concurrent GC pass but strictly better than not copying at all — so this
// degrades rather than failing.
func beginMirrorLease(c *client.Client, cacheCtx context.Context, log *slog.Logger) (func(), context.Context) {
	lm := c.LeasesService()
	if lm == nil {
		return func() {}, cacheCtx
	}
	l, err := lm.Create(cacheCtx,
		leases.WithRandomID(),
		leases.WithExpiration(mirrorLeaseTTL),
	)
	if err != nil {
		log.Debug("dind cache: lease unavailable, mirroring unleased", "error", err)
		return func() {}, cacheCtx
	}
	return func() {
		// Non-synchronous delete: by now the Image record roots the
		// content, so there is nothing to wait for.
		if derr := lm.Delete(cacheCtx, l); derr != nil && !errdefs.IsNotFound(derr) {
			log.Debug("dind cache: releasing mirror lease", "lease", l.ID, "error", derr)
		}
	}, leases.WithLease(cacheCtx, l.ID)
}

// mirrorContentRefs walks the image DAG rooted at target in the source
// namespace and gives the destination namespace its own content reference
// (blob bucket) for every descriptor. Returns how many descriptors were
// handled.
func mirrorContentRefs(srcCtx, dstCtx context.Context, cs content.Store, target ocispec.Descriptor, log *slog.Logger) (int, error) {
	if cs == nil {
		return 0, fmt.Errorf("no content store")
	}
	descs, err := presentDescriptors(srcCtx, cs, target)
	if err != nil {
		return 0, err
	}
	for _, d := range descs {
		info, ierr := cs.Info(srcCtx, d.Digest)
		if ierr != nil {
			if errdefs.IsNotFound(ierr) {
				continue
			}
			return 0, fmt.Errorf("source info %s: %w", d.Digest, ierr)
		}
		if cerr := copyContentRef(srcCtx, dstCtx, cs, d, info.Labels, log); cerr != nil {
			return 0, fmt.Errorf("copy content ref %s: %w", d.Digest, cerr)
		}
	}
	return len(descs), nil
}

// presentDescriptors returns every descriptor reachable from root whose blob
// actually exists in the given namespace's content store, root first.
//
// Absent descriptors are skipped, not errors: a multi-platform index names
// manifests for platforms this node never pulled, and images can carry
// foreign/URL layers that are never in the local store.
func presentDescriptors(ctx context.Context, cs content.Store, root ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	var (
		out   []ocispec.Descriptor
		seen  = map[digest.Digest]bool{}
		queue = []ocispec.Descriptor{root}
	)
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		if d.Digest == "" || seen[d.Digest] {
			continue
		}
		seen[d.Digest] = true

		if _, err := cs.Info(ctx, d.Digest); err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("info %s: %w", d.Digest, err)
		}
		out = append(out, d)

		children, err := images.Children(ctx, cs, d)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("children of %s: %w", d.Digest, err)
		}
		queue = append(queue, children...)
	}
	return out, nil
}

// copyContentRef makes desc resolvable in dstCtx's namespace, carrying over
// the source blob's labels — which include the containerd.io/gc.ref.content.*
// entries the mark phase follows from a manifest to its config and layers.
// Without those the manifest bucket would survive a GC pass and its children
// would not.
func copyContentRef(srcCtx, dstCtx context.Context, cs content.Store, desc ocispec.Descriptor, labels map[string]string, log *slog.Logger) error {
	// Already bucketed in the destination (re-mirror of a cached tag).
	// Still refresh the labels so an entry written by an older ephemerd,
	// or one whose gc.ref labels were incomplete, gets repaired.
	if _, err := cs.Info(dstCtx, desc.Digest); err == nil {
		if len(labels) == 0 {
			return nil
		}
		if _, uerr := cs.Update(dstCtx, content.Info{
			Digest: desc.Digest,
			Labels: labels,
		}, "labels"); uerr != nil && !errdefs.IsNotFound(uerr) {
			return fmt.Errorf("refresh labels: %w", uerr)
		}
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("destination info: %w", err)
	}

	w, err := cs.Writer(dstCtx,
		content.WithRef("ephemerd-dind-cache-"+desc.Digest.String()),
		content.WithDescriptor(desc),
	)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("open writer: %w", err)
	}
	defer func() {
		// Close after a successful Commit is a no-op; anything else here
		// is an ingest that will expire on its own, so log and move on.
		if cerr := w.Close(); cerr != nil {
			log.Debug("dind cache: closing content writer", "digest", desc.Digest, "error", cerr)
		}
	}()

	// Under the default "shared" sharing policy the writer comes back
	// already at full offset and Commit just creates the bucket. Under
	// "isolated" it doesn't, and we have to supply the bytes — read them
	// back out of the source namespace rather than the network.
	if st, serr := w.Status(); serr == nil && st.Offset != desc.Size {
		ra, rerr := cs.ReaderAt(srcCtx, desc)
		if rerr != nil {
			return fmt.Errorf("source reader: %w", rerr)
		}
		defer func() {
			if cerr := ra.Close(); cerr != nil {
				log.Debug("dind cache: closing source reader", "digest", desc.Digest, "error", cerr)
			}
		}()
		if cerr := content.CopyReaderAt(w, ra, desc.Size); cerr != nil {
			return fmt.Errorf("copy blob: %w", cerr)
		}
	}

	if cerr := w.Commit(dstCtx, desc.Size, desc.Digest, content.WithLabels(labels)); cerr != nil {
		if errdefs.IsAlreadyExists(cerr) {
			return nil
		}
		return fmt.Errorf("commit: %w", cerr)
	}
	return nil
}

// RefreshLastAccessed bumps the LastAccessedLabel on a cached image. Called
// from the container-create path when a job references an image that's
// already in the cache (no pull happens, but the image is in use). Silently
// no-ops if the image isn't in the cache.
//
// Delegates to imagegc.Touch, which does the same thing for the main
// runtime namespace — one writer for the LRU key.
func RefreshLastAccessed(ctx context.Context, c *client.Client, cacheNS, imageName string, log *slog.Logger) {
	imagegc.Touch(ctx, c, cacheNS, imageName, log)
}

// CachePrune walks every per-repo cache namespace and evicts Image records
// whose LastAccessedLabel (or UpdatedAt fallback for records pre-dating the
// label) is older than maxAge, then deletes any cache namespace left empty.
// Containerd's content GC reclaims the unreferenced blobs after this runs.
//
// maxAge <= 0 skips eviction and runs only the empty-namespace reap. That is
// now the default: age is an optional backstop, and disk-pressure-triggered
// LRU collection (pkg/imagegc, wired from [image_gc]) is the primary
// mechanism for keeping these namespaces bounded. Keeping the reap
// unconditional means a node that disables the age backstop still doesn't
// accumulate one stale metadata bucket per repo that ever ran a job.
//
// The candidate listing, protection and ordering all come from pkg/imagegc
// so this path and the pressure collector cannot drift apart.
//
// Returns nil and logs warnings on partial failures — the next pass will
// retry whatever didn't clean up this time.
func CachePrune(ctx context.Context, c *client.Client, maxAge time.Duration, log *slog.Logger) error {
	if c == nil {
		return nil
	}
	all, err := c.NamespaceService().List(ctx)
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}

	var cacheNS []string
	for _, ns := range all {
		if strings.HasPrefix(ns, DindCacheNamespacePrefix) {
			cacheNS = append(cacheNS, ns)
		}
	}
	if len(cacheNS) == 0 {
		return nil
	}

	totalEvicted := 0
	if maxAge > 0 {
		// Never evict an image some container still references, even
		// here: the age backstop has no idea what is in flight.
		running, rerr := imagegc.RunningImageRefs(ctx, c, log)
		if rerr != nil {
			return fmt.Errorf("listing running container images: %w", rerr)
		}
		protected := imagegc.ProtectedSet(nil, running)

		cands, cerr := imagegc.ListCandidates(ctx, c, cacheNS, log)
		if cerr != nil {
			return fmt.Errorf("listing cache image records: %w", cerr)
		}
		aged := imagegc.PlanByAge(cands, protected, time.Now().UTC().Add(-maxAge))
		totalEvicted = imagegc.Evict(ctx, c, aged, false, log, nil)
		if totalEvicted > 0 {
			log.Info("cache prune: evicted images",
				"count", totalEvicted, "max_age", maxAge)
		}
	}

	// Drop the metadata bucket of any cache namespace left with no image
	// records, so it doesn't accumulate one stale bucket per repo that
	// ever ran a job, even if the repo itself goes idle.
	namespacesPruned := 0
	for _, ns := range cacheNS {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		remaining, lerr := c.ImageService().List(nsCtx)
		if lerr != nil {
			log.Warn("cache prune: list images", "namespace", ns, "error", lerr)
			continue
		}
		if len(remaining) == 0 {
			CleanupJobNamespace(ctx, c, ns, log)
			namespacesPruned++
		}
	}

	if totalEvicted > 0 || namespacesPruned > 0 {
		log.Info("cache prune: complete",
			"images_evicted", totalEvicted, "namespaces_pruned", namespacesPruned)
	}
	return nil
}
