// Docker build API → BuildKit translator.
//
// Wires POST /build into the embedded BuildKit solver from pkg/buildkit.
// See docs/arch/docker-builds.md for the design.
//
// This file is build-tag-free; it works on every platform where pkg/buildkit
// has a worker (linux, windows). On darwin pkg/buildkit returns an error
// from NewServer so the handler is simply unreachable.

package dind

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ephpm/ephemerd/pkg/buildkit"
	bkclient "github.com/moby/buildkit/client"
)

// bufioReader wraps r so we can Peek at the first bytes without consuming.
func bufioReader(r io.Reader) *bufio.Reader {
	if br, ok := r.(*bufio.Reader); ok {
		return br
	}
	return bufio.NewReader(r)
}

// isGzip peeks at the magic bytes and reports whether the stream starts
// with a gzip header. Does not consume the bytes.
func isGzip(br *bufio.Reader) bool {
	b, err := br.Peek(2)
	if err != nil {
		return false
	}
	return b[0] == 0x1f && b[1] == 0x8b
}

// handleImageBuildBuildkit is an alternative handler for POST /build that
// routes through the embedded BuildKit solver instead of buildah. Not yet
// wired into the default router — callers plug it in by assigning
// s.buildHandler when they're ready to switch.
//
// The existing handleImageBuild (buildah on Linux, 501 elsewhere) stays
// in place until BuildKit MVP ships.
func (s *Server) handleImageBuildBuildkit(bk *buildkit.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// BuildKit panics on internal errors (e.g. typeurl marshal failures)
		// can otherwise propagate up the goroutine and kill the whole
		// daemon. Recover here so a build failure stays scoped to the
		// build request — the client gets a 500 and ephemerd keeps polling.
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("buildkit handler panic recovered", "panic", rec)
				_ = writeJSONLine(w, map[string]any{
					"errorDetail": map[string]any{"message": fmt.Sprintf("internal buildkit panic: %v", rec)},
					"error":       fmt.Sprintf("internal buildkit panic: %v", rec),
				})
			}
		}()

		if bk == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"message": "buildkit not configured",
			})
			return
		}

		// 1. Extract build context to a temp dir.
		ctxDir, err := extractBuildContextToTempDir(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"message": fmt.Sprintf("build context: %v", err),
			})
			return
		}
		defer func() {
			if err := os.RemoveAll(ctxDir); err != nil {
				s.log.Warn("cleanup build context", "dir", ctxDir, "error", err)
			}
		}()

		// 2. Translate Docker build options → SolveOpt.
		opt, err := dockerBuildOptsToSolveOpt(r, ctxDir)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"message": fmt.Sprintf("build options: %v", err),
			})
			return
		}

		// 3. Stream progress to the client in Docker jsonmessage format.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		statusCh := make(chan *bkclient.SolveStatus, 16)
		errCh := make(chan error, 1)

		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					s.log.Error("buildkit Build goroutine panic recovered", "panic", rec)
					errCh <- fmt.Errorf("internal buildkit panic: %v", rec)
				}
			}()
			_, err := bk.Build(r.Context(), opt, statusCh)
			errCh <- err
		}()

		for status := range statusCh {
			if err := writeSolveStatus(w, status); err != nil {
				s.log.Warn("writing build status", "error", err)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}

		if err := <-errCh; err != nil {
			_ = writeJSONLine(w, map[string]any{
				"errorDetail": map[string]any{"message": err.Error()},
				"error":       err.Error(),
			})
			return
		}

		_ = writeJSONLine(w, map[string]any{
			"stream": "Successfully built\n",
		})
	}
}

// extractBuildContextToTempDir reads a (optionally gzipped) tar stream and extracts
// it to a fresh temp directory. Returns the directory path. On any error after
// the temp dir is created, the dir is removed and any cleanup failure is folded
// into the returned error so callers don't need a separate cleanup hook.
func extractBuildContextToTempDir(r io.Reader) (resultDir string, retErr error) {
	dir, err := os.MkdirTemp("", "ephemerd-build-*")
	if err != nil {
		return "", fmt.Errorf("mkdir temp: %w", err)
	}
	defer func() {
		if retErr == nil {
			return
		}
		if rerr := os.RemoveAll(dir); rerr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("cleanup %s: %w", dir, rerr))
		}
	}()

	br := bufioReader(r)

	var tr *tar.Reader
	if isGzip(br) {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return "", fmt.Errorf("gzip reader: %w", err)
		}
		defer func() {
			if cerr := gz.Close(); cerr != nil && retErr == nil {
				retErr = fmt.Errorf("gzip close: %w", cerr)
			}
		}()
		tr = tar.NewReader(gz)
	} else {
		tr = tar.NewReader(br)
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar next: %w", err)
		}

		// Reject path traversal attempts.
		if strings.Contains(hdr.Name, "..") {
			return "", fmt.Errorf("invalid tar entry: %s", hdr.Name)
		}
		target := filepath.Join(dir, hdr.Name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", fmt.Errorf("mkdir parent %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return "", fmt.Errorf("open %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				if cerr := f.Close(); cerr != nil {
					return "", errors.Join(fmt.Errorf("copy %s: %w", target, err), fmt.Errorf("close %s: %w", target, cerr))
				}
				return "", fmt.Errorf("copy %s: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return "", fmt.Errorf("close %s: %w", target, err)
			}
		default:
			// Skip symlinks, devices, etc. for now — add later if needed.
		}
	}
	return dir, nil
}

// dockerBuildOptsToSolveOpt converts Docker build API query parameters into
// a BuildKit SolveOpt. This is the heart of the arch doc's "option
// translation" table. Not complete — MVP subset: -t, -f, --target,
// --build-arg, --no-cache, --pull, --label.
func dockerBuildOptsToSolveOpt(r *http.Request, ctxDir string) (bkclient.SolveOpt, error) {
	q := r.URL.Query()

	dockerfile := q.Get("dockerfile")
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	target := q.Get("target")
	// docker build -t a -t b → ?t=a&t=b. q["t"] preserves all values; the
	// older q.Get("t") only returned the first, so the second tag was
	// never registered in containerd and the subsequent docker push for
	// that tag 404'd.
	tags := q["t"]
	noCache := q.Get("nocache") == "1"
	pull := q.Get("pull") == "1"

	frontendAttrs := map[string]string{
		"filename": dockerfile,
	}
	if target != "" {
		frontendAttrs["target"] = target
	}
	if noCache {
		frontendAttrs["no-cache"] = ""
	}
	if pull {
		frontendAttrs["image-resolve-mode"] = "pull"
	}

	// --build-arg: Docker sends these as a JSON-encoded map in the
	// `buildargs` query param.
	if raw := q.Get("buildargs"); raw != "" {
		var args map[string]string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return bkclient.SolveOpt{}, fmt.Errorf("buildargs json: %w", err)
		}
		for k, v := range args {
			frontendAttrs["build-arg:"+k] = v
		}
	}

	// --label: same encoding as buildargs.
	if raw := q.Get("labels"); raw != "" {
		var labels map[string]string
		if err := json.Unmarshal([]byte(raw), &labels); err != nil {
			return bkclient.SolveOpt{}, fmt.Errorf("labels json: %w", err)
		}
		for k, v := range labels {
			frontendAttrs["label:"+k] = v
		}
	}

	// --platform: Docker sends one platform; BuildKit accepts a
	// comma-separated list.
	if plat := q.Get("platform"); plat != "" {
		frontendAttrs["platform"] = plat
	}

	// Image export: write the result into ephemerd's containerd image
	// store under the requested tag(s). buildkit's image exporter accepts
	// a comma-separated list under "name" — register all of them so a
	// subsequent docker push <tag> finds the image regardless of which
	// -t the user picked. Registry push happens later via the existing
	// pkg/dind push path, not during build.
	exportAttrs := map[string]string{}
	if len(tags) > 0 {
		exportAttrs["name"] = strings.Join(tags, ",")
	}
	exports := []bkclient.ExportEntry{{
		Type:  "image",
		Attrs: exportAttrs,
	}}

	return bkclient.SolveOpt{
		Exports:       exports,
		Frontend:      "dockerfile.v0",
		FrontendAttrs: frontendAttrs,
		// LocalMounts: build context + dockerfile directory. Both point at
		// the same ctxDir today; buildkit treats them as separate mounts
		// so callers can put the Dockerfile outside the build context.
		// TODO: use fsutil.FS instead of LocalDirs (deprecated).
		LocalDirs: map[string]string{
			"context":    ctxDir,
			"dockerfile": ctxDir,
		},
	}, nil
}

// writeSolveStatus translates one BuildKit SolveStatus into Docker
// jsonmessage lines and writes them to w. This is a minimal mapping —
// full fidelity (progress bars, cache hits, SBOM) is phase 2 work.
func writeSolveStatus(w io.Writer, status *bkclient.SolveStatus) error {
	for _, vtx := range status.Vertexes {
		msg := map[string]any{}
		if vtx.Error != "" {
			msg["stream"] = fmt.Sprintf("%s: %s\n", vtx.Name, vtx.Error)
		} else {
			msg["stream"] = fmt.Sprintf("%s\n", vtx.Name)
		}
		if err := writeJSONLine(w, msg); err != nil {
			return err
		}
	}
	for _, log := range status.Logs {
		msg := map[string]any{
			"stream": string(log.Data),
		}
		if err := writeJSONLine(w, msg); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n"))
	return err
}
