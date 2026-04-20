//go:build !linux

package containerd

import "runtime"

// extractShims returns the data directory on Windows so containerd can find
// containerd-shim-runhcs-v1.exe on PATH. On other platforms it's a no-op.
func extractShims(dataDir string) (string, func(), error) {
	if runtime.GOOS == "windows" {
		return dataDir, func() {}, nil
	}
	return "", func() {}, nil
}
