// Package artifacts handles OCI image layer extraction for macOS VM jobs.
//
// When a macOS VM job specifies `container: { image: ... }` in its workflow,
// ephemerd pulls the OCI image and extracts all layers into a flat directory
// on the host filesystem. This is NOT running a container -- just unpacking
// the filesystem layers into a regular directory that is shared with the
// macOS VM via virtio-fs.
//
// The extraction uses go-containerregistry (crane) to pull images directly
// from the registry, avoiding any dependency on containerd's snapshotter.
// This is essential on Darwin hosts where containerd runs inside a Linux VM
// and its snapshotters (overlayfs, erofs) cannot operate on the host filesystem.
package artifacts

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Extractor pulls OCI images and extracts their layers into host directories.
// It uses go-containerregistry to pull directly from the registry, avoiding
// containerd's snapshotter which requires Linux filesystem operations.
type Extractor struct {
	log *slog.Logger
}

// NewExtractor creates an artifact extractor. The containerd client parameter
// is accepted for backward compatibility but is no longer used -- extraction
// now uses go-containerregistry to pull directly from the registry.
func NewExtractor(log *slog.Logger) *Extractor {
	return &Extractor{
		log: log,
	}
}

// Extract pulls the OCI image from the registry and extracts all layers
// into destDir. The directory is created if it does not exist. Each layer is
// applied in order on top of the previous, producing the final merged filesystem.
func (e *Extractor) Extract(ctx context.Context, imageRef string, destDir string) error {
	e.log.Info("extracting OCI image artifacts", "image", imageRef, "dest", destDir)

	// Parse the image reference.
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("parsing image reference %s: %w", imageRef, err)
	}

	// Pull the image descriptor and manifest from the registry.
	desc, err := remote.Get(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("fetching image %s: %w", imageRef, err)
	}

	img, err := desc.Image()
	if err != nil {
		return fmt.Errorf("resolving image %s: %w", imageRef, err)
	}

	// Ensure the destination directory exists.
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating artifacts directory %s: %w", destDir, err)
	}

	// Get the image layers.
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("reading image layers: %w", err)
	}

	if len(layers) == 0 {
		e.log.Warn("image has no layers", "image", imageRef)
		return nil
	}

	e.log.Debug("extracting layers", "image", imageRef, "count", len(layers))

	// Extract each layer in order. go-containerregistry handles decompression
	// automatically via the Uncompressed() method.
	for i, layer := range layers {
		if err := e.extractLayer(layer, destDir, i); err != nil {
			return fmt.Errorf("extracting layer %d: %w", i, err)
		}
	}

	e.log.Info("artifacts extracted", "image", imageRef, "dest", destDir, "layers", len(layers))
	return nil
}

// extractLayer reads a single layer and extracts its tar contents into destDir.
func (e *Extractor) extractLayer(layer v1.Layer, destDir string, index int) error {
	digest, err := layer.Digest()
	if err != nil {
		return fmt.Errorf("reading layer digest: %w", err)
	}

	size, err := layer.Size()
	if err != nil {
		return fmt.Errorf("reading layer size: %w", err)
	}

	e.log.Debug("applying layer", "index", index, "digest", digest, "size", size)

	// Uncompressed() returns the decompressed tar stream.
	rc, err := layer.Uncompressed()
	if err != nil {
		return fmt.Errorf("opening layer content: %w", err)
	}
	defer func() {
		if closeErr := rc.Close(); closeErr != nil {
			e.log.Warn("failed to close layer reader", "index", index, "error", closeErr)
		}
	}()

	// Apply the tar archive to the destination directory.
	if err := applyTar(rc, destDir); err != nil {
		return fmt.Errorf("applying tar layer %d: %w", index, err)
	}

	return nil
}

// applyTar extracts a tar stream into destDir, creating files and directories
// as needed. It handles whiteout files (.wh.) for layer deletion semantics.
func applyTar(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tar header: %w", err)
		}

		// Sanitize the path to prevent directory traversal.
		cleanName := filepath.Clean(hdr.Name)
		if strings.HasPrefix(cleanName, "..") {
			continue // skip entries that try to escape the destination
		}

		target := filepath.Join(destDir, cleanName)

		// Handle OCI whiteout files: .wh.<name> means delete <name>.
		base := filepath.Base(cleanName)
		if strings.HasPrefix(base, ".wh.") {
			deleteName := strings.TrimPrefix(base, ".wh.")
			deleteTarget := filepath.Join(filepath.Dir(target), deleteName)
			if err := os.RemoveAll(deleteTarget); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("applying whiteout for %s: %w", deleteName, err)
			}
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return fmt.Errorf("creating directory %s: %w", cleanName, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("creating parent directory for %s: %w", cleanName, err)
			}
			if err := writeFile(target, tr, os.FileMode(hdr.Mode)); err != nil {
				return fmt.Errorf("writing file %s: %w", cleanName, err)
			}
		case tar.TypeSymlink:
			// Remove existing file/symlink before creating.
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing existing symlink target %s: %w", cleanName, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("creating parent directory for symlink %s: %w", cleanName, err)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("creating symlink %s -> %s: %w", cleanName, hdr.Linkname, err)
			}
		case tar.TypeLink:
			// Hard link — resolve relative to destDir.
			linkTarget := filepath.Join(destDir, filepath.Clean(hdr.Linkname))
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing existing hard link target %s: %w", cleanName, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("creating parent directory for link %s: %w", cleanName, err)
			}
			if err := os.Link(linkTarget, target); err != nil {
				return fmt.Errorf("creating hard link %s -> %s: %w", cleanName, hdr.Linkname, err)
			}
		default:
			// Skip block devices, char devices, fifos, etc. that don't apply
			// to artifact extraction on the host filesystem.
		}
	}
}

// writeFile creates or truncates a file and copies the content from r.
func writeFile(path string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(f, r)
	closeErr := f.Close()

	if copyErr != nil {
		return copyErr
	}
	return closeErr
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
