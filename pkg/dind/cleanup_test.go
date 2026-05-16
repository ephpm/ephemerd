//go:build !darwin

package dind

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestCleanup_DindNamespaces drives both cleanup helpers against a single
// embedded containerd. Combined into one TestMain-style function with
// subtests because containerd's prometheus metrics use a process-global
// registry — spinning up two containerd.New() instances in the same
// process panics with "duplicate metrics collector registration".
//
// Subtests share the containerd but use distinct namespace names so they
// don't bleed into each other.
//
// Regression test for the disk-fill leak (73 leaked dind namespaces
// pinning ~1 GB of image content each on a 100 GB VHDX).
func TestCleanup_DindNamespaces(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cleanup test in short mode")
	}

	c := sharedTestContainerd(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Containerd namespace identifiers max out at 76 chars, so test
	// namespace names need to stay short — well under what production
	// uses (ephemerd-dind-ephemerd-github-<repo>-<runner-name>, ~50 chars).
	t.Run("RemovesImageLeaseAndNamespace", func(t *testing.T) {
		ns := DindNamespacePrefix + "test-image-lease"
		nsCtx := namespaces.WithNamespace(context.Background(), ns)

		// Populate with one Image record and one Lease.
		imgRecord := images.Image{
			Name: "example.test/dind/leak:tag",
			Target: ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    digest.FromString("dind-cleanup-test-manifest"),
				Size:      42,
			},
		}
		ctx, cancel := context.WithTimeout(nsCtx, 30*time.Second)
		defer cancel()
		if _, err := c.ImageService().Create(ctx, imgRecord); err != nil {
			t.Fatalf("create image: %v", err)
		}
		lease, err := c.LeasesService().Create(ctx, leases.WithExpiration(5*time.Minute))
		if err != nil {
			t.Fatalf("create lease: %v", err)
		}

		// Sanity: namespace visible before cleanup.
		before, lerr := c.NamespaceService().List(context.Background())
		if lerr != nil {
			t.Fatalf("list namespaces (pre): %v", lerr)
		}
		if !slices.Contains(before, ns) {
			t.Fatalf("namespace %q missing before cleanup; got %v", ns, before)
		}

		CleanupJobNamespace(context.Background(), c, ns, log)

		after, lerr := c.NamespaceService().List(context.Background())
		if lerr != nil {
			t.Fatalf("list namespaces (post): %v", lerr)
		}
		if slices.Contains(after, ns) {
			t.Errorf("namespace %q still present after cleanup; got %v", ns, after)
		}
		gone := namespaces.WithNamespace(context.Background(), ns)
		if _, gerr := c.ImageService().Get(gone, imgRecord.Name); gerr == nil {
			t.Errorf("image %q still resolvable after cleanup", imgRecord.Name)
		}
		if ls, lerr := c.LeasesService().List(gone); lerr == nil {
			for _, l := range ls {
				if l.ID == lease.ID {
					t.Errorf("lease %q still present after cleanup", lease.ID)
				}
			}
		}
	})

	t.Run("StaleSweepFiltersByPrefix", func(t *testing.T) {
		dindNS := DindNamespacePrefix + "stale-sweep"
		keepNS := "keep-me-buildkit-style"

		for _, ns := range []string{dindNS, keepNS} {
			nsCtx, cancel := context.WithTimeout(
				namespaces.WithNamespace(context.Background(), ns),
				30*time.Second,
			)
			if _, err := c.LeasesService().Create(nsCtx, leases.WithExpiration(5*time.Minute)); err != nil {
				cancel()
				t.Fatalf("create lease in %s: %v", ns, err)
			}
			cancel()
		}

		CleanupStaleDindNamespaces(context.Background(), c, log)

		list, err := c.NamespaceService().List(context.Background())
		if err != nil {
			t.Fatalf("list namespaces: %v", err)
		}
		if slices.Contains(list, dindNS) {
			t.Errorf("dind namespace %q should have been cleaned; got %v", dindNS, list)
		}
		if !slices.Contains(list, keepNS) {
			t.Errorf("non-dind namespace %q was wrongly removed; got %v", keepNS, list)
		}
		for _, ns := range list {
			// Cache namespaces (ephemerd-dind-cache-*) are intentionally
			// preserved by CleanupStaleDindNamespaces — they're long-lived
			// per-repo image caches and are managed by CachePrune, not the
			// stale-job sweeper.
			if strings.HasPrefix(ns, DindNamespacePrefix) && !strings.HasPrefix(ns, DindCacheNamespacePrefix) {
				t.Errorf("leftover dind-prefixed (non-cache) namespace after cleanup: %q", ns)
			}
		}
	})
}
