//go:build !linux

package dind

// chownNewDirsLikeAncestor is a no-op outside Linux. Ownership inheritance
// only matters in production (in-VM Linux); cross-platform tests don't
// care about uid/gid on the auto-created tempdirs they generate.
func chownNewDirsLikeAncestor(newDirs []string, ancestor string) error {
	return nil
}
