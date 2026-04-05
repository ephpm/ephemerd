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

// extractShims extracts the embedded shim and runc binaries next to the
// ephemerd binary so containerd can find them.
func extractShims() (func(), error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("finding executable path: %w", err)
	}
	dir := filepath.Dir(self)

	for _, name := range shimBinaries {
		dst := filepath.Join(dir, name)

		// Skip if already exists
		if _, err := os.Stat(dst); err == nil {
			continue
		}

		data, err := shimFS.ReadFile(filepath.Join("embed", name))
		if err != nil {
			return nil, fmt.Errorf("reading embedded %s: %w", name, err)
		}

		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return nil, fmt.Errorf("writing %s: %w", dst, err)
		}
	}

	// Don't delete shim binaries on shutdown — they're needed for orphan
	// cleanup on restart (containerd calls the shim to delete dead containers).
	return func() {}, nil
}
