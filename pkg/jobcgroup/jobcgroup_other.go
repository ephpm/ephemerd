//go:build !linux

package jobcgroup

import (
	"log/slog"
	"time"
)

// Ensure is a no-op off Linux: job cgroups are a Linux cgroup v2 construct.
func Ensure(_, _ string, _ Limits) error { return nil }

// Remove is a no-op off Linux.
func Remove(_, _ string) error { return nil }

// SweepStale is a no-op off Linux.
func SweepStale(_ string, _ time.Duration, _ time.Time, _ map[string]bool) (int, error) {
	return 0, nil
}

// Sweep is a no-op off Linux.
func Sweep(_ time.Duration, _ map[string]bool, _ *slog.Logger) {}
