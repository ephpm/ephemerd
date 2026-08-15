// Package diskspace reports filesystem capacity for a path using a single
// statfs-class syscall (statfs(2) on Linux/macOS, GetDiskFreeSpaceExW on
// Windows). A Check call costs microseconds, so callers may poll it on a
// short interval or inline it on a hot path such as "before pulling an
// image".
//
// It deliberately does NOT walk directories. A du-style traversal of
// <data-dir>/containerd is minutes of I/O on a node with hundreds of
// overlay snapshots and would itself become a source of load — the whole
// point of this helper is that asking the filesystem is free.
//
// The per-OS implementations live in usage_unix.go, usage_windows.go and
// usage_unsupported.go; they were lifted from the equivalent checks in
// cmd/ephemerd/doctor_*.go.
package diskspace

import "fmt"

// Usage is a point-in-time capacity reading for the filesystem backing a
// path.
//
// FreeBytes is the space available to the *calling user*, not the raw free
// count: on Linux/macOS that is statfs.Bavail (which excludes the
// root-reserved blocks) and on Windows it is GetDiskFreeSpaceExW's
// lpFreeBytesAvailable (which respects quotas). UsedBytes is therefore
// derived as Total-Free and counts reserved space as used — deliberately
// conservative, since ephemerd does not run as a user that can spend the
// reserve.
type Usage struct {
	// Path is the path that was measured (not necessarily a mount point).
	Path string
	// TotalBytes is the filesystem's total capacity.
	TotalBytes uint64
	// FreeBytes is the space available to this process.
	FreeBytes uint64
}

// UsedBytes returns capacity minus space available to this process.
func (u Usage) UsedBytes() uint64 {
	if u.FreeBytes >= u.TotalBytes {
		return 0
	}
	return u.TotalBytes - u.FreeBytes
}

// UsedPercent returns used capacity as a percentage in [0,100]. A zero-size
// filesystem (unreadable, or a stub value from a failed probe) reports 0 so
// callers never trigger garbage collection on a division artifact.
func (u Usage) UsedPercent() float64 {
	if u.TotalBytes == 0 {
		return 0
	}
	return float64(u.UsedBytes()) / float64(u.TotalBytes) * 100
}

// FreePercent returns available capacity as a percentage in [0,100].
func (u Usage) FreePercent() float64 {
	if u.TotalBytes == 0 {
		return 0
	}
	return float64(u.FreeBytes) / float64(u.TotalBytes) * 100
}

// String renders a compact human-readable summary for log messages.
func (u Usage) String() string {
	return fmt.Sprintf("%s: %.1f GiB free of %.1f GiB (%.1f%% used)",
		u.Path, GiB(u.FreeBytes), GiB(u.TotalBytes), u.UsedPercent())
}

// GiB converts a byte count to gibibytes. Exported because every caller
// that logs a byte count wants the same conversion.
func GiB(b uint64) float64 {
	return float64(b) / (1024 * 1024 * 1024)
}

// Check returns a capacity reading for the filesystem containing path.
// path need not be a mount point; any existing file or directory on the
// filesystem works.
func Check(path string) (Usage, error) {
	return check(path)
}
