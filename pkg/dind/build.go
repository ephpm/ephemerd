//go:build linux

package dind

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/containers/buildah/define"
	"github.com/containers/buildah/imagebuildah"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	is "go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/types"
)

// handleImageBuild handles POST /build — builds an OCI image from a
// Dockerfile + build context tar stream. Uses buildah as a Go library.
func (s *Server) handleImageBuild(w http.ResponseWriter, r *http.Request) {
	if s.client == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"message": "containerd client not available",
		})
		return
	}

	// Parse query params (Docker build API).
	dockerfile := r.URL.Query().Get("dockerfile")
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	tag := r.URL.Query().Get("t")
	noCache := r.URL.Query().Get("nocache") == "1"
	target := r.URL.Query().Get("target")

	// Stream progress to the client.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	writeProgress := func(stream string) {
		msg := map[string]string{"stream": stream + "\n"}
		data, err := json.Marshal(msg)
		if err != nil {
			return
		}
		if _, err := w.Write(data); err != nil {
			return
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	writeError := func(errMsg string) {
		msg := map[string]string{"error": errMsg}
		data, _ := json.Marshal(msg)
		_, _ = w.Write(data)
		_, _ = w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Extract build context from the request body (tar or tar.gz).
	contextDir, err := os.MkdirTemp("", "dind-build-*")
	if err != nil {
		writeError(fmt.Sprintf("creating build context dir: %v", err))
		return
	}
	defer func() { _ = os.RemoveAll(contextDir) }()

	writeProgress("Step 0: Extracting build context")

	if err := extractBuildContext(r.Body, contextDir); err != nil {
		writeError(fmt.Sprintf("extracting build context: %v", err))
		return
	}

	// Verify the Dockerfile exists in the context.
	dockerfilePath := filepath.Join(contextDir, dockerfile)
	if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
		writeError(fmt.Sprintf("dockerfile %q not found in build context", dockerfile))
		return
	}

	// Initialize containers/storage for buildah. Each job gets its own
	// storage root under the dind docker directory for isolation.
	storeDir := filepath.Join(filepath.Dir(s.sockPath), "buildah-store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		writeError(fmt.Sprintf("creating store dir: %v", err))
		return
	}

	storeOpts, err := types.DefaultStoreOptions()
	if err != nil {
		writeError(fmt.Sprintf("getting default store options: %v", err))
		return
	}
	storeOpts.GraphRoot = filepath.Join(storeDir, "graph")
	storeOpts.RunRoot = filepath.Join(storeDir, "run")
	storeOpts.GraphDriverName = "overlay"

	store, err := storage.GetStore(storeOpts)
	if err != nil {
		writeError(fmt.Sprintf("initializing storage: %v", err))
		return
	}
	defer func() {
		if _, err := store.Shutdown(false); err != nil {
			s.log.Debug("storage shutdown", "error", err)
		}
	}()

	// Build options.
	buildOpts := define.BuildOptions{
		ContextDirectory: contextDir,
		NoCache:          noCache,
		Target:           target,
		Output:           tag,
		SystemContext:     &is.SystemContext{},
		// Log output via our progress writer.
		Out:    &progressWriter{write: writeProgress},
		Err:    &progressWriter{write: writeProgress},
		Labels: []string{},
	}

	if tag != "" {
		buildOpts.AdditionalTags = []string{tag}
	}

	writeProgress(fmt.Sprintf("Building %s", dockerfile))

	// Run the build.
	imageID, _, err := imagebuildah.BuildDockerfiles(
		r.Context(), store, buildOpts, dockerfilePath,
	)
	if err != nil {
		writeError(fmt.Sprintf("build failed: %v", err))
		return
	}

	writeProgress(fmt.Sprintf("Successfully built %s", imageID))
	if tag != "" {
		writeProgress(fmt.Sprintf("Successfully tagged %s", tag))
	}

	// Register the built image in our in-memory map so docker images
	// and docker run can find it.
	ref := tag
	if ref == "" {
		ref = imageID
	}
	s.mu.Lock()
	s.images[ref] = &imageEntry{
		ID:  imageID,
		Ref: ref,
	}
	s.mu.Unlock()

	// Also tag in the per-job containerd namespace so docker run can
	// resolve it. We need to import from buildah's storage into containerd.
	if s.client != nil && tag != "" {
		jobCtx := namespaces.WithNamespace(r.Context(), s.jobNamespace)
		// Try to import the image from buildah's storage into containerd.
		// This is best-effort — if it fails, the image is still in
		// buildah's store and can be pushed from there.
		if img, err := store.Image(imageID); err == nil {
			s.log.Info("built image available", "id", imageID, "tag", tag, "layers", len(img.BigDataNames))
			_ = jobCtx // future: import OCI layout into containerd
		}
	}

	s.log.Info("image built", "id", imageID, "tag", tag)
}

// progressWriter adapts a log function to io.Writer for buildah output.
type progressWriter struct {
	write func(string)
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	lines := strings.Split(strings.TrimRight(string(p), "\n"), "\n")
	for _, line := range lines {
		if line != "" {
			pw.write(line)
		}
	}
	return len(p), nil
}

// extractBuildContext extracts a tar (or tar.gz) stream into a directory.
func extractBuildContext(r io.Reader, dst string) error {
	// Docker sends the build context as a tar or tar.gz stream.
	// Try gzip first; if it fails, treat as plain tar.
	buf := make([]byte, 2)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("reading header: %w", err)
	}

	// Reconstruct the reader with the peeked bytes.
	fullReader := io.MultiReader(strings.NewReader(string(buf[:n])), r)

	var tarReader *tar.Reader
	if n >= 2 && buf[0] == 0x1f && buf[1] == 0x8b {
		// gzip magic number
		gz, err := gzip.NewReader(fullReader)
		if err != nil {
			return fmt.Errorf("gzip reader: %w", err)
		}
		defer func() { _ = gz.Close() }()
		tarReader = tar.NewReader(gz)
	} else {
		tarReader = tar.NewReader(fullReader)
	}

	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}

		target := filepath.Join(dst, filepath.Clean(hdr.Name))

		// Prevent path traversal.
		if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) && target != filepath.Clean(dst) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tarReader); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}
