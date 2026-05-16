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
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestCacheNamespace_FormatAndIsolation(t *testing.T) {
	tests := []struct {
		name, provider, repo, want string
	}{
		{"github simple", "github", "ephpm/ephpm", "ephemerd-dind-cache-github-ephpm_ephpm"},
		{"gitea same name not collision", "gitea", "ephpm/ephpm", "ephemerd-dind-cache-gitea-ephpm_ephpm"},
		{"gitlab nested", "gitlab", "acme/platform/api", "ephemerd-dind-cache-gitlab-acme_platform_api"},
		{"upper preserved", "GitHub", "Org/Repo", "ephemerd-dind-cache-GitHub-Org_Repo"},
		{"weird chars sanitized", "github", "owner/repo@1", "ephemerd-dind-cache-github-owner_repo_1"},
		{"leading separator trimmed", "github", "/leading", "ephemerd-dind-cache-github-leading"},
		{"trailing separator trimmed", "github", "trailing/", "ephemerd-dind-cache-github-trailing"},
		{"empty provider", "", "any/repo", ""},
		{"empty repo", "github", "", ""},
		{"only-bad chars", "github", "////", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CacheNamespace(tc.provider, tc.repo)
			if got != tc.want {
				t.Errorf("CacheNamespace(%q, %q) = %q, want %q", tc.provider, tc.repo, got, tc.want)
			}
		})
	}

	// Cross-provider isolation invariant: same repo path on different
	// providers MUST produce distinct namespaces.
	if a, b := CacheNamespace("github", "ephpm/ephpm"), CacheNamespace("gitea", "ephpm/ephpm"); a == b {
		t.Errorf("cross-provider collision: github and gitea both produced %q", a)
	}
	// Same-provider distinct repos isolated too.
	if a, b := CacheNamespace("github", "foo/bar"), CacheNamespace("github", "foo/baz"); a == b {
		t.Errorf("same-provider distinct repos collided: %q", a)
	}
}

// TestCache_MirrorAndPrune drives the full cache lifecycle: mirror an
// image record into a per-repo cache namespace, refresh its last-accessed,
// then exercise CachePrune to evict an artificially-aged record.
func TestCache_MirrorAndPrune(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cache test in short mode")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c := sharedTestContainerd(t)

	const (
		provider = "github"
		repo     = "ephpm/ephpm"
		jobID    = "ephemerd-github-ephpm-test-mirror"
		imgName  = "ghcr.io/ephpm/cache-test:1.0"
	)
	jobNS := DindNamespacePrefix + "test-mirror"
	cacheNS := CacheNamespace(provider, repo)
	if cacheNS != "ephemerd-dind-cache-github-ephpm_ephpm" {
		t.Fatalf("unexpected cacheNS: %q", cacheNS)
	}

	// Stage an Image record in the per-job namespace, then mirror it.
	jobCtx, cancel := context.WithTimeout(
		namespaces.WithNamespace(context.Background(), jobNS),
		60*time.Second,
	)
	defer cancel()
	imgRecord := images.Image{
		Name: imgName,
		Target: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    digest.FromString("cache-mirror-test-manifest"),
			Size:      256,
		},
	}
	if _, err := c.ImageService().Create(jobCtx, imgRecord); err != nil {
		t.Fatalf("create job image: %v", err)
	}

	if err := MirrorImageToCache(context.Background(), c, jobNS, cacheNS, imgName, log); err != nil {
		t.Fatalf("MirrorImageToCache: %v", err)
	}

	cacheCtx := namespaces.WithNamespace(context.Background(), cacheNS)
	mirrored, err := c.ImageService().Get(cacheCtx, imgName)
	if err != nil {
		t.Fatalf("get cache image: %v", err)
	}
	if mirrored.Labels[LastAccessedLabel] == "" {
		t.Errorf("last-accessed label not set after mirror; labels=%v", mirrored.Labels)
	}

	// Idempotent: mirroring again should not error and should refresh the label.
	firstTS := mirrored.Labels[LastAccessedLabel]
	time.Sleep(1100 * time.Millisecond) // RFC3339 is second-precision
	if err := MirrorImageToCache(context.Background(), c, jobNS, cacheNS, imgName, log); err != nil {
		t.Fatalf("re-mirror: %v", err)
	}
	again, _ := c.ImageService().Get(cacheCtx, imgName)
	if again.Labels[LastAccessedLabel] == firstTS {
		t.Errorf("last-accessed didn't advance on re-mirror: still %q", firstTS)
	}

	// Force-age the cache record so CachePrune evicts it. Set label to
	// 10 days ago; prune threshold of 7 days should trip.
	old := time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339)
	again.Labels[LastAccessedLabel] = old
	if _, err := c.ImageService().Update(cacheCtx, again, "labels"); err != nil {
		t.Fatalf("backdate label: %v", err)
	}

	if err := CachePrune(context.Background(), c, 7*24*time.Hour, log); err != nil {
		t.Fatalf("CachePrune: %v", err)
	}

	// After prune the image record should be gone, and the now-empty
	// cache namespace should have been removed too.
	if _, err := c.ImageService().Get(cacheCtx, imgName); err == nil {
		t.Errorf("image still present after prune")
	}
	list, lerr := c.NamespaceService().List(context.Background())
	if lerr != nil {
		t.Fatalf("list namespaces: %v", lerr)
	}
	if slices.Contains(list, cacheNS) {
		t.Errorf("empty cache namespace %q should have been cleaned up; got %v", cacheNS, list)
	}

	// Job ns is untouched by cache prune.
	if _, err := c.ImageService().Get(jobCtx, imgName); err != nil {
		t.Errorf("job-namespace image was incorrectly removed: %v", err)
	}
}

// TestCachePrune_KeepsFreshAndPrefixedOnly verifies CachePrune doesn't
// touch fresh entries OR non-cache namespaces.
func TestCachePrune_KeepsFreshAndPrefixedOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cache test in short mode")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c := sharedTestContainerd(t)

	cacheNS := CacheNamespace("github", "ephpm/keep-fresh")
	otherNS := "non-cache-namespace-keep"

	cacheCtx := namespaces.WithNamespace(context.Background(), cacheNS)
	otherCtx := namespaces.WithNamespace(context.Background(), otherNS)

	// Fresh cache image — last-accessed = now.
	fresh := images.Image{
		Name: "ghcr.io/ephpm/fresh:tag",
		Target: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    digest.FromString("cache-prune-fresh-manifest"),
			Size:      99,
		},
		Labels: map[string]string{LastAccessedLabel: time.Now().UTC().Format(time.RFC3339)},
	}
	if _, err := c.ImageService().Create(cacheCtx, fresh); err != nil {
		t.Fatalf("create fresh cache image: %v", err)
	}
	// Non-cache namespace with an image — must not be touched.
	other := images.Image{
		Name: "ghcr.io/something/else:v1",
		Target: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    digest.FromString("cache-prune-other-manifest"),
			Size:      99,
		},
	}
	if _, err := c.ImageService().Create(otherCtx, other); err != nil {
		t.Fatalf("create other-ns image: %v", err)
	}

	if err := CachePrune(context.Background(), c, 7*24*time.Hour, log); err != nil {
		t.Fatalf("CachePrune: %v", err)
	}

	if _, err := c.ImageService().Get(cacheCtx, fresh.Name); err != nil {
		t.Errorf("fresh cache image was incorrectly evicted: %v", err)
	}
	if _, err := c.ImageService().Get(otherCtx, other.Name); err != nil {
		t.Errorf("non-cache image was incorrectly touched: %v", err)
	}
}

// TestSanitizeForNamespace_CollapsesAndTrims is a focused unit test on the
// sanitization helper so the cross-provider isolation invariant has a
// dedicated regression target.
func TestSanitizeForNamespace_CollapsesAndTrims(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		"plain":               "plain",
		"with-dashes":         "with-dashes",
		"with.dots":           "with.dots",
		"slash/inside":        "slash_inside",
		"a//b":                "a_b",
		"___leading":          "leading",
		"trailing___":         "trailing",
		"!!!only-special!!!":  "only-special",
		"acme/platform/api":   "acme_platform_api",
	}
	for in, want := range cases {
		if got := sanitizeForNamespace(in); got != want {
			t.Errorf("sanitizeForNamespace(%q) = %q, want %q", in, got, want)
		}
	}
	// Containerd's namespace identifier regex: alphanumerics with
	// ._ - separators, no consecutive separators, no leading/trailing.
	for in := range cases {
		got := sanitizeForNamespace(in)
		if got == "" {
			continue
		}
		if strings.HasPrefix(got, "_") || strings.HasPrefix(got, "-") || strings.HasPrefix(got, ".") {
			t.Errorf("sanitizeForNamespace(%q) = %q starts with separator", in, got)
		}
		if strings.HasSuffix(got, "_") || strings.HasSuffix(got, "-") || strings.HasSuffix(got, ".") {
			t.Errorf("sanitizeForNamespace(%q) = %q ends with separator", in, got)
		}
	}
}
