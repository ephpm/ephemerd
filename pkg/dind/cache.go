package dind

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/ephpm/ephemerd/pkg/imagegc"
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

// MirrorImageToCache copies an Image record from the per-job namespace into
// the per-repo cache namespace (creating it if needed), refreshing the
// LastAccessedLabel on the cache record. The underlying content blobs are
// already in the global content store from the original pull; this only
// adds metadata so the cache record's gc.ref labels keep the content alive
// after the per-job namespace is cleaned up.
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
	log.Debug("dind cache: mirrored image", "image", imageName, "cache", cacheNS)
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
