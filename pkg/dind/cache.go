package dind

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
const LastAccessedLabel = "ephemerd.io/last-accessed"

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
func RefreshLastAccessed(ctx context.Context, c *client.Client, cacheNS, imageName string, log *slog.Logger) {
	if c == nil || cacheNS == "" || imageName == "" {
		return
	}
	cacheCtx := namespaces.WithNamespace(ctx, cacheNS)
	img, err := c.ImageService().Get(cacheCtx, imageName)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			log.Debug("dind cache: refresh get", "image", imageName, "cache", cacheNS, "error", err)
		}
		return
	}
	if img.Labels == nil {
		img.Labels = map[string]string{}
	}
	img.Labels[LastAccessedLabel] = time.Now().UTC().Format(time.RFC3339)
	if _, err := c.ImageService().Update(cacheCtx, img, "labels"); err != nil {
		log.Debug("dind cache: refresh update", "image", imageName, "cache", cacheNS, "error", err)
	}
}

// CachePrune walks every per-repo cache namespace and evicts Image records
// whose LastAccessedLabel (or CreatedAt fallback for records pre-dating the
// label) is older than maxAge. Empty cache namespaces are deleted entirely.
// Containerd's content GC reclaims the unreferenced blobs after this runs.
//
// Returns nil and logs warnings on partial failures — the next pass will
// retry whatever didn't clean up this time.
func CachePrune(ctx context.Context, c *client.Client, maxAge time.Duration, log *slog.Logger) error {
	if c == nil || maxAge <= 0 {
		return nil
	}
	all, err := c.NamespaceService().List(ctx)
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}

	cutoff := time.Now().UTC().Add(-maxAge)
	totalEvicted := 0
	namespacesPruned := 0

	for _, ns := range all {
		if !strings.HasPrefix(ns, DindCacheNamespacePrefix) {
			continue
		}
		nsCtx := namespaces.WithNamespace(ctx, ns)
		imgs, ierr := c.ImageService().List(nsCtx)
		if ierr != nil {
			log.Warn("cache prune: list images", "namespace", ns, "error", ierr)
			continue
		}
		evicted := 0
		for _, img := range imgs {
			ts := imageLastAccessed(img)
			if ts.IsZero() || ts.After(cutoff) {
				continue
			}
			if derr := c.ImageService().Delete(nsCtx, img.Name); derr != nil && !errdefs.IsNotFound(derr) {
				log.Warn("cache prune: delete image",
					"namespace", ns, "image", img.Name, "error", derr)
				continue
			}
			evicted++
		}
		if evicted > 0 {
			log.Info("cache prune: evicted images",
				"namespace", ns, "count", evicted, "max_age", maxAge)
		}
		totalEvicted += evicted

		// If the cache namespace is now empty, drop the metadata bucket
		// too so it doesn't accumulate one stale bucket per repo that
		// ever ran a job, even if the repo itself goes idle.
		remaining, lerr := c.ImageService().List(nsCtx)
		if lerr == nil && len(remaining) == 0 {
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

// imageLastAccessed returns the timestamp the image was last used. Prefers
// the LastAccessedLabel; falls back to img.UpdatedAt for records that pre-
// date the label (so existing caches don't get nuked on the first prune
// after this code lands).
func imageLastAccessed(img images.Image) time.Time {
	if ts := img.Labels[LastAccessedLabel]; ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t.UTC()
		}
	}
	return img.UpdatedAt.UTC()
}
