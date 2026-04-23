// Package artifacts handles OCI image layer extraction for macOS VM jobs.
//
// When a macOS VM job specifies `container: { image: ... }` in its workflow,
// ephemerd pulls the OCI image via containerd and extracts all layers into a
// flat directory on the host filesystem. This is NOT running a container --
// just unpacking the filesystem layers into a regular directory that is shared
// with the macOS VM via virtio-fs.
package artifacts

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const namespace = "ephemerd"

// Extractor pulls OCI images and extracts their layers into host directories.
type Extractor struct {
	client *client.Client
	log    *slog.Logger
}

// NewExtractor creates an artifact extractor using the given containerd client.
func NewExtractor(c *client.Client, log *slog.Logger) *Extractor {
	return &Extractor{
		client: c,
		log:    log,
	}
}

// Extract pulls the OCI image (if not already cached) and extracts all layers
// into destDir. The directory is created if it does not exist. Each layer is
// applied in order on top of the previous, producing the final merged filesystem.
func (e *Extractor) Extract(ctx context.Context, imageRef string, destDir string) error {
	ctx = namespaces.WithNamespace(ctx, namespace)

	e.log.Info("extracting OCI image artifacts", "image", imageRef, "dest", destDir)

	// Pull or get cached image.
	img, err := e.client.GetImage(ctx, imageRef)
	if err != nil {
		e.log.Info("image not cached, pulling", "image", imageRef)
		img, err = e.client.Pull(ctx, imageRef, client.WithPullUnpack)
		if err != nil {
			return fmt.Errorf("pulling image %s: %w", imageRef, err)
		}
	}

	// Ensure the destination directory exists.
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating artifacts directory %s: %w", destDir, err)
	}

	// Resolve the manifest for the current platform to get layer descriptors.
	store := e.client.ContentStore()
	manifest, err := images.Manifest(ctx, store, img.Target(), platforms.Default())
	if err != nil {
		return fmt.Errorf("resolving image manifest: %w", err)
	}

	if len(manifest.Layers) == 0 {
		e.log.Warn("image has no layers", "image", imageRef)
		return nil
	}

	e.log.Debug("extracting layers", "image", imageRef, "count", len(manifest.Layers))

	// Extract each layer in order. Layers are typically compressed tar archives.
	// We read the raw content from containerd's content store, decompress, and
	// apply the tar to the destination directory.
	for i, layer := range manifest.Layers {
		if err := e.extractLayer(ctx, store, layer, destDir, i); err != nil {
			return fmt.Errorf("extracting layer %d (%s): %w", i, layer.Digest, err)
		}
	}

	e.log.Info("artifacts extracted", "image", imageRef, "dest", destDir, "layers", len(manifest.Layers))
	return nil
}

// extractLayer reads a single layer from the content store and extracts it
// into destDir. It handles decompression (gzip, zstd, etc.) automatically.
func (e *Extractor) extractLayer(ctx context.Context, store content.Store, layer ocispec.Descriptor, destDir string, index int) error {
	e.log.Debug("applying layer", "index", index, "digest", layer.Digest, "size", layer.Size, "mediaType", layer.MediaType)

	ra, err := store.ReaderAt(ctx, layer)
	if err != nil {
		return fmt.Errorf("opening layer content: %w", err)
	}
	defer func() { _ = ra.Close() }()

	// Convert ReaderAt to Reader for decompression.
	reader := content.NewReader(ra)

	// Decompress the layer stream (handles gzip, zstd, and uncompressed).
	ds, err := compression.DecompressStream(reader)
	if err != nil {
		return fmt.Errorf("decompressing layer: %w", err)
	}
	defer func() { _ = ds.Close() }()

	// Apply the tar archive to the destination directory.
	if _, err := archive.Apply(ctx, destDir, ds); err != nil {
		return fmt.Errorf("applying tar layer: %w", err)
	}

	return nil
}

// ArtifactsDir returns the artifacts directory for a given job under the data directory.
func ArtifactsDir(dataDir string, jobID string) string {
	return filepath.Join(dataDir, "artifacts", jobID)
}

// Cleanup removes the artifacts directory for a job. It is safe to call even
// if the directory does not exist.
func Cleanup(dir string, log *slog.Logger) {
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Warn("failed to remove artifacts directory", "path", dir, "error", err)
	} else {
		log.Debug("artifacts directory cleaned up", "path", dir)
	}
}

// ListContents returns a list of file paths in the artifacts directory (for debugging).
func ListContents(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel != "." {
			files = append(files, rel)
		}
		return nil
	})
	return files, err
}

