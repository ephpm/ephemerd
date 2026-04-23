//go:build windows

package containerd

import (
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

//go:embed embed/containerd-shim-runhcs-v1.exe
var shimFS embed.FS

var shimBinaries = []string{"containerd-shim-runhcs-v1.exe"}

// extractShims extracts the embedded Windows container runtime shim into the
// data directory so containerd can find it via PATH.
func extractShims(dataDir string) (string, func(), error) {
	dir := filepath.Join(dataDir, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("creating bin directory: %w", err)
	}

	for _, name := range shimBinaries {
		dst := filepath.Join(dir, name)

		// Skip if already exists
		if _, err := os.Stat(dst); err == nil {
			continue
		}

		data, err := shimFS.ReadFile("embed/" + name)
		if err != nil {
			return "", nil, fmt.Errorf("reading embedded %s: %w", name, err)
		}

		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return "", nil, fmt.Errorf("writing %s: %w", dst, err)
		}
	}

	return dir, func() {
		for _, name := range shimBinaries {
			p := filepath.Join(dir, name)
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				slog.Warn("cleanup: removing embedded shim", "path", p, "error", err)
			}
		}
	}, nil
}
