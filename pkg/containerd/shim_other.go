//go:build !linux

package containerd

// extractShims is a no-op on non-Linux platforms.
// Windows uses Hyper-V and macOS uses Virtualization.framework.
func extractShims(_ string) (string, func(), error) {
	return "", func() {}, nil
}
