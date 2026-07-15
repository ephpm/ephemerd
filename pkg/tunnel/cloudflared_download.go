package tunnel

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const cloudflaredDownloadTimeout = 5 * time.Minute

// cloudflaredAsset returns the release download URL and whether the asset is
// a gzipped tarball. Cloudflare publishes bare binaries for Linux and Windows
// but ships macOS as cloudflared-darwin-<arch>.tgz (a gzip tar containing the
// `cloudflared` binary), so the darwin path must extract rather than chmod the
// downloaded file directly.
func cloudflaredAsset(version string) (url string, isTgz bool) {
	base := "https://github.com/cloudflare/cloudflared/releases/download/" + version + "/cloudflared-"
	switch runtime.GOOS {
	case "darwin":
		return fmt.Sprintf("%s%s-%s.tgz", base, runtime.GOOS, runtime.GOARCH), true
	case "windows":
		return fmt.Sprintf("%s%s-%s.exe", base, runtime.GOOS, runtime.GOARCH), false
	default:
		return fmt.Sprintf("%s%s-%s", base, runtime.GOOS, runtime.GOARCH), false
	}
}

// ensureCloudflaredBinary returns the path to the cloudflared binary,
// downloading it into <dir>/bin/cloudflared if not already present.
// Idempotent: subsequent calls no-op after the first successful install.
func ensureCloudflaredBinary(ctx context.Context, dir, version string) (string, error) {
	binDir := filepath.Join(dir, "bin")
	binPath := filepath.Join(binDir, cloudflaredBinName())
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("cloudflared: mkdir %s: %w", binDir, err)
	}

	url, isTgz := cloudflaredAsset(version)
	slog.Info("downloading cloudflared", "version", version, "url", url, "dest", binPath)

	dlCtx, cancel := context.WithTimeout(ctx, cloudflaredDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("cloudflared: build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cloudflared: download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("cloudflared: download %s: %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(binDir, "cloudflared-*.tmp")
	if err != nil {
		return "", fmt.Errorf("cloudflared: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	var writeErr error
	if isTgz {
		writeErr = extractCloudflaredTgz(resp.Body, tmp)
	} else {
		_, writeErr = io.Copy(tmp, resp.Body)
	}
	if writeErr != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("cloudflared: write body: %w", writeErr)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		cleanup()
		return "", err
	}
	if err := os.Rename(tmpPath, binPath); err != nil {
		cleanup()
		return "", fmt.Errorf("cloudflared: rename to %s: %w", binPath, err)
	}
	slog.Info("cloudflared installed", "path", binPath)
	return binPath, nil
}

// extractCloudflaredTgz reads a gzipped tar and writes the `cloudflared`
// entry to dst. The macOS release archive contains a single top-level
// `cloudflared` binary.
func extractCloudflaredTgz(r io.Reader, dst io.Writer) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("no cloudflared entry in archive")
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if filepath.Base(hdr.Name) == "cloudflared" && hdr.Typeflag == tar.TypeReg {
			if _, err := io.Copy(dst, tr); err != nil { //nolint:gosec // release archive, bounded size
				return fmt.Errorf("extract cloudflared: %w", err)
			}
			return nil
		}
	}
}

func cloudflaredBinName() string {
	if runtime.GOOS == "windows" {
		return "cloudflared.exe"
	}
	return "cloudflared"
}
