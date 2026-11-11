//go:build !linux

package runtime

// matchTreeOwner is a Linux overlayfs concern; the Windows path byte-copies
// the tree and macOS runs the Linux runtime inside the VM.
func matchTreeOwner(string, string) error { return nil }
