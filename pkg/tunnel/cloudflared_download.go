package tunnel

import (
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

const (
	cloudflaredDownloadTimeout = 5 * time.Minute
	// The direct-binary release naming Cloudflare publishes for each tag.
	// linux/amd64, linux/arm64, darwin/amd64, darwin/arm64. Windows would need
	// ".exe"; we don't bother because ephemerd only runs cloudflared on Linux
	// hosts today.
	cloudflaredReleaseURL = "https://github.com/cloudflare/cloudflared/releases/download/%s/cloudflared-%s-%s"
)

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

	url := fmt.Sprintf(cloudflaredReleaseURL, version, runtime.GOOS, runtime.GOARCH)
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
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("cloudflared: download %s: %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(binDir, "cloudflared-*.tmp")
	if err != nil {
		return "", fmt.Errorf("cloudflared: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// If we bail before rename, don't leave a stale temp file around.
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("cloudflared: write body: %w", err)
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

func cloudflaredBinName() string {
	if runtime.GOOS == "windows" {
		return "cloudflared.exe"
	}
	return "cloudflared"
}
