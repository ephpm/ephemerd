//go:build !darwin

package dind

// Regression tests for #138: the per-repo dind image cache retained an Image
// record but no content, so containerd's GC collected the blobs as soon as
// the per-job namespace went away and the next job re-downloaded the image.
//
// These tests drive the real embedded containerd (shared across the package
// via sharedTestContainerd) and force real garbage collection, because the
// property under test — "a blob survives a GC pass" — only exists inside
// containerd's metadata store.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// stagedImage is a synthetic single-layer OCI image written straight into a
// namespace's content store — manifest, config and layer blobs plus an Image
// record — standing in for what client.Pull leaves behind. Building it by
// hand keeps the test hermetic (no registry) while still exercising the real
// containerd metadata store and GC.
type stagedImage struct {
	name          string
	manifest      ocispec.Descriptor
	config        ocispec.Descriptor
	layer         ocispec.Descriptor
	manifestBytes []byte
}

// all returns the image's descriptors, root first.
func (s stagedImage) all() []ocispec.Descriptor {
	return []ocispec.Descriptor{s.manifest, s.config, s.layer}
}

func stageImage(t *testing.T, c *client.Client, ns, name, salt string) stagedImage {
	t.Helper()
	ctx := namespaces.WithNamespace(context.Background(), ns)
	cs := c.ContentStore()

	layerBytes := []byte("ephemerd-dind-cache-test-layer-" + salt + strings.Repeat("L", 8192))
	layer := descFor(ocispec.MediaTypeImageLayer, layerBytes)
	writeTestBlob(t, ctx, cs, layer, layerBytes, nil)

	configBytes, err := json.Marshal(ocispec.Image{
		Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: []digest.Digest{layer.Digest},
		},
		Config: ocispec.ImageConfig{Cmd: []string{"/bin/true", salt}},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	config := descFor(ocispec.MediaTypeImageConfig, configBytes)
	writeTestBlob(t, ctx, cs, config, configBytes, nil)

	manifestBytes, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers:    []ocispec.Descriptor{layer},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifest := descFor(ocispec.MediaTypeImageManifest, manifestBytes)

	// These are the labels containerd's own pull path stamps on a manifest
	// blob. The GC mark phase walks manifest -> config + layers through
	// them; without them on the mirrored copy the children get swept.
	writeTestBlob(t, ctx, cs, manifest, manifestBytes, map[string]string{
		gcRefConfigLabel: config.Digest.String(),
		gcRefLayer0Label: layer.Digest.String(),
	})

	if _, err := c.ImageService().Create(ctx, images.Image{Name: name, Target: manifest}); err != nil {
		t.Fatalf("create image %q in %s: %v", name, ns, err)
	}

	return stagedImage{
		name:          name,
		manifest:      manifest,
		config:        config,
		layer:         layer,
		manifestBytes: manifestBytes,
	}
}

const (
	gcRefConfigLabel = "containerd.io/gc.ref.content.config"
	gcRefLayer0Label = "containerd.io/gc.ref.content.l.0"
)

func descFor(mediaType string, b []byte) ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.FromBytes(b),
		Size:      int64(len(b)),
	}
}

func writeTestBlob(t *testing.T, ctx context.Context, cs content.Store, desc ocispec.Descriptor, b []byte, labels map[string]string) {
	t.Helper()
	var opts []content.Opt
	if len(labels) > 0 {
		opts = append(opts, content.WithLabels(labels))
	}
	ref := "ephemerd-test-" + desc.Digest.String()
	if err := content.WriteBlob(ctx, cs, ref, bytes.NewReader(b), desc, opts...); err != nil {
		t.Fatalf("write blob %s: %v", desc.Digest, err)
	}
}

// forceContainerdGC runs a full, synchronous containerd garbage collection.
//
// Creating and synchronously deleting a lease is the API-level GC trigger
// containerd documents (docs/garbage-collection.md; it is what
// `ctr content prune` does): the leases service turns Sync into
// leases.SynchronousDelete, which runs metadata.DB.GarbageCollect — mark and
// sweep over the metadata buckets, then the content store's backing-blob
// sweep — before returning.
func forceContainerdGC(t *testing.T, c *client.Client) {
	t.Helper()
	ctx := namespaces.WithNamespace(context.Background(), "ephemerd-test-gc-trigger")
	lm := c.LeasesService()
	l, err := lm.Create(ctx, leases.WithRandomID())
	if err != nil {
		t.Fatalf("create gc-trigger lease: %v", err)
	}
	if err := lm.Delete(ctx, l, leases.SynchronousDelete); err != nil {
		t.Fatalf("synchronous lease delete (gc trigger): %v", err)
	}
}

// contentIsLocallyAvailable answers the question that actually drives the
// bandwidth bill: if a brand-new job namespace pulled this blob right now,
// would containerd download it?
//
// It reproduces what remotes.fetch does — open a content writer for the
// descriptor and look at the offset. containerd's metadata content store
// short-circuits when the blob is already in the backing store and the
// sharing policy allows it, handing back a writer already at full offset;
// remotes.fetch sees ws.Offset == desc.Size and skips the network entirely.
// Offset 0 means a real download.
//
// The probe is always aborted: a short-circuited ingest records the expected
// digest, and contentStore.garbageCollect treats that as a reason to keep the
// blob, which would pin the very thing later assertions want collected.
func contentIsLocallyAvailable(t *testing.T, c *client.Client, desc ocispec.Descriptor) bool {
	t.Helper()
	ctx := namespaces.WithNamespace(context.Background(), "ephemerd-test-pull-probe")
	cs := c.ContentStore()
	ref := fmt.Sprintf("probe-%s-%d", desc.Digest.Encoded()[:12], time.Now().UnixNano())

	w, err := cs.Writer(ctx, content.WithRef(ref), content.WithDescriptor(desc))
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			// A bucket already exists in the probe namespace itself.
			return true
		}
		t.Fatalf("probe writer for %s: %v", desc.Digest, err)
	}
	st, serr := w.Status()
	_ = w.Close()
	if serr != nil {
		_ = cs.Abort(ctx, ref)
		t.Fatalf("probe status for %s: %v", desc.Digest, serr)
	}
	hit := st.Offset == desc.Size

	aerr := cs.Abort(ctx, ref)
	switch {
	case aerr == nil || errdefs.IsNotFound(aerr):
	case hit:
		// A short-circuited probe is pure metadata — no ingest directory on
		// disk — so aborting it must work. Failing to means the expected
		// digest stays recorded and pins the blob against later GC passes.
		t.Fatalf("abort short-circuited probe ingest %s: %v", ref, aerr)
	default:
		// A miss allocated a real (empty) ingest directory, which Windows
		// sometimes refuses to unlink straight away. It records no expected
		// digest, so it pins nothing and leaving it behind is harmless.
		t.Logf("probe ingest %s left behind (harmless, records no digest): %v", ref, aerr)
	}
	return hit
}

// TestCache_MirrorRetainsContentAcrossGC is the regression test for #138.
//
// Two identically-shaped images are staged in two per-job namespaces. Only
// one is mirrored into a per-repo cache namespace. Both job namespaces are
// then torn down the way a finished job tears them down, and real containerd
// garbage collection is forced.
//
// The un-mirrored image is the negative control. If its blobs survived too,
// the GC would not be collecting anything and "the mirrored blobs survived"
// would prove nothing. Before the fix the mirrored image behaved exactly like
// the control: the cache namespace got an Image record but no content
// buckets, and containerd's contentStore.garbageCollect deletes a backing
// blob the moment no namespace holds a bucket for it.
func TestCache_MirrorRetainsContentAcrossGC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping containerd-backed cache test in short mode")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c := sharedTestContainerd(t)
	ctx := context.Background()
	cs := c.ContentStore()

	const (
		cachedName  = "ghcr.io/ephpm/retain-cached:1.0"
		controlName = "ghcr.io/ephpm/retain-control:1.0"
	)
	cachedJobNS := DindNamespacePrefix + "retain-cached-job"
	controlJobNS := DindNamespacePrefix + "retain-control-job"
	cacheNS := CacheNamespace("github", "ephpm/retain")
	cacheCtx := namespaces.WithNamespace(ctx, cacheNS)

	t.Cleanup(func() {
		CleanupJobNamespace(ctx, c, cacheNS, log)
		CleanupJobNamespace(ctx, c, cachedJobNS, log)
		CleanupJobNamespace(ctx, c, controlJobNS, log)
	})

	cached := stageImage(t, c, cachedJobNS, cachedName, "cached")
	control := stageImage(t, c, controlJobNS, controlName, "control")

	// Only one of them goes through the cache.
	if err := MirrorImageToCache(ctx, c, cachedJobNS, cacheNS, cachedName, log); err != nil {
		t.Fatalf("MirrorImageToCache: %v", err)
	}

	// The mirror must have given the cache namespace its own content
	// references. An Image record on its own is exactly what #138 was.
	for _, d := range cached.all() {
		if _, err := cs.Info(cacheCtx, d.Digest); err != nil {
			t.Fatalf("cache namespace holds no content reference for %s (%s): %v",
				d.MediaType, d.Digest, err)
		}
	}

	// Finish both "jobs".
	CleanupJobNamespace(ctx, c, cachedJobNS, log)
	CleanupJobNamespace(ctx, c, controlJobNS, log)

	// Collect. Bounded retry because containerd only sweeps the backing
	// content store when the metadata pass marked it dirty, and the shared
	// test daemon's background scheduler may have consumed that flag first.
	collected := false
	for i := 0; i < 3 && !collected; i++ {
		forceContainerdGC(t, c)
		collected = !contentIsLocallyAvailable(t, c, control.layer)
	}
	if !collected {
		t.Fatal("negative control failed: content from the un-mirrored job namespace " +
			"survived 3 GC passes, so this test cannot prove the cache retains anything")
	}

	// Every blob of the mirrored image must have survived the same passes.
	for _, d := range cached.all() {
		if _, err := cs.Info(cacheCtx, d.Digest); err != nil {
			t.Errorf("cached %s (%s) lost its cache-namespace content reference after GC: %v",
				d.MediaType, d.Digest, err)
		}
		if !contentIsLocallyAvailable(t, c, d) {
			t.Errorf("cached %s (%s) is gone from the backing content store after GC — "+
				"the next job for this repo would re-download it", d.MediaType, d.Digest)
		}
	}

	// ...and be readable, not merely recorded.
	got, err := content.ReadBlob(cacheCtx, cs, cached.manifest)
	if err != nil {
		t.Fatalf("read mirrored manifest from cache namespace: %v", err)
	}
	if !bytes.Equal(got, cached.manifestBytes) {
		t.Errorf("mirrored manifest bytes differ: got %d bytes, want %d",
			len(got), len(cached.manifestBytes))
	}

	// The Image record survives too, so LRU pruning still has something to
	// age out.
	if _, err := c.ImageService().Get(cacheCtx, cachedName); err != nil {
		t.Errorf("cache image record missing after GC: %v", err)
	}
}

// TestCache_MirrorIsIdempotent re-mirrors an image whose content is already
// in the cache namespace. The repeat passes must be no-ops that still repair
// the gc.ref labels, not AlreadyExists failures.
func TestCache_MirrorIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping containerd-backed cache test in short mode")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c := sharedTestContainerd(t)
	ctx := context.Background()
	cs := c.ContentStore()

	const imgName = "ghcr.io/ephpm/retain-idempotent:1.0"
	jobNS := DindNamespacePrefix + "retain-idempotent-job"
	cacheNS := CacheNamespace("github", "ephpm/retain-idempotent")
	cacheCtx := namespaces.WithNamespace(ctx, cacheNS)

	t.Cleanup(func() {
		CleanupJobNamespace(ctx, c, cacheNS, log)
		CleanupJobNamespace(ctx, c, jobNS, log)
	})

	staged := stageImage(t, c, jobNS, imgName, "idempotent")

	for i := range 3 {
		if err := MirrorImageToCache(ctx, c, jobNS, cacheNS, imgName, log); err != nil {
			t.Fatalf("mirror pass %d: %v", i+1, err)
		}
	}

	info, err := cs.Info(cacheCtx, staged.manifest.Digest)
	if err != nil {
		t.Fatalf("cache manifest info: %v", err)
	}
	// The gc.ref labels are the edge from the manifest to its children.
	// Losing them on a re-mirror would silently unpin config and layers.
	if got := info.Labels[gcRefConfigLabel]; got != staged.config.Digest.String() {
		t.Errorf("config gc.ref label = %q, want %q", got, staged.config.Digest.String())
	}
	if got := info.Labels[gcRefLayer0Label]; got != staged.layer.Digest.String() {
		t.Errorf("layer gc.ref label = %q, want %q", got, staged.layer.Digest.String())
	}
}

// TestMirrorImageToCache_ToleratesMissingContent covers the degenerate case
// the old implementation was permanently in: an Image record whose target is
// not in the local content store. Mirroring must still record the image so
// LRU/prune bookkeeping keeps working, rather than failing the job's pull.
func TestMirrorImageToCache_ToleratesMissingContent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping containerd-backed cache test in short mode")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c := sharedTestContainerd(t)
	ctx := context.Background()

	const imgName = "ghcr.io/ephpm/no-content:1.0"
	jobNS := DindNamespacePrefix + "retain-nocontent-job"
	cacheNS := CacheNamespace("github", "ephpm/retain-nocontent")

	t.Cleanup(func() {
		CleanupJobNamespace(ctx, c, cacheNS, log)
		CleanupJobNamespace(ctx, c, jobNS, log)
	})

	jobCtx := namespaces.WithNamespace(ctx, jobNS)
	if _, err := c.ImageService().Create(jobCtx, images.Image{
		Name: imgName,
		Target: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    digest.FromString("retain-nocontent-manifest"),
			Size:      128,
		},
	}); err != nil {
		t.Fatalf("create job image: %v", err)
	}

	if err := MirrorImageToCache(ctx, c, jobNS, cacheNS, imgName, log); err != nil {
		t.Fatalf("MirrorImageToCache with absent content: %v", err)
	}
	if _, err := c.ImageService().Get(namespaces.WithNamespace(ctx, cacheNS), imgName); err != nil {
		t.Errorf("image record not mirrored: %v", err)
	}
}
