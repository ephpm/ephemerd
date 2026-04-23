//go:build !linux && !windows

package containerd

// extractShims is a no-op on platforms that don't need embedded shims
// (e.g. macOS where the VM runs Linux containerd internally).
func extractShims(_ string) (string, func(), error) {
	return "", func() {}, nil
}
