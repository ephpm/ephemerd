//go:build linux

package containerd

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed embed/containerd-shim-runc-v2 embed/runc
var shimFS embed.FS

var shimBinaries = []string{"containerd-shim-runc-v2", "runc"}

// extractShims extracts the embedded shim and runc binaries into the data
// directory so containerd can find them via PATH.
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

		data, err := shimFS.ReadFile(filepath.Join("embed", name))
		if err != nil {
			return "", nil, fmt.Errorf("reading embedded %s: %w", name, err)
		}

		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return "", nil, fmt.Errorf("writing %s: %w", dst, err)
		}
	}

	return dir, func() {
		for _, name := range shimBinaries {
			os.Remove(filepath.Join(dir, name))
		}
	}, nil
}
