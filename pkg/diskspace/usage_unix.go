//go:build linux || darwin

package diskspace

import (
	"fmt"
	"syscall"
)

// check reads capacity via statfs(2). Bsize differs in width between Linux
// (int64) and Darwin (uint32), so both are widened to uint64 before the
// multiply — the same conversion cmd/ephemerd/doctor_{linux,darwin}.go use.
func check(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{Path: path}, fmt.Errorf("statfs %s: %w", path, err)
	}
	bsize := uint64(st.Bsize)
	return Usage{
		Path:       path,
		TotalBytes: uint64(st.Blocks) * bsize,
		FreeBytes:  uint64(st.Bavail) * bsize,
	}, nil
}
