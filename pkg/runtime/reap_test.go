package runtime

import (
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
)

// TestOrphanContainerReapable exercises the safety predicate that decides
// whether ReapDeadContainers may delete a container. A false positive here
// destroys a running CI job, so the cases below pin down each guard: only a
// provably-dead container past the grace window, and not mid-provision, is
// selected.
func TestOrphanContainerReapable(t *testing.T) {
	const grace = 10 * time.Minute
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	longAgo := now.Add(-1 * time.Hour) // well past grace
	justNow := now.Add(-30 * time.Second)

	tests := []struct {
		name         string
		hasTask      bool
		status       client.ProcessStatus
		exitTime     time.Time
		createdAt    time.Time
		provisioning bool
		want         bool
	}{
		{
			name:      "stopped task past grace is reapable",
			hasTask:   true,
			status:    client.Stopped,
			exitTime:  longAgo,
			createdAt: longAgo,
			want:      true,
		},
		{
			name:      "absent task, old container is reapable",
			hasTask:   false,
			createdAt: longAgo,
			want:      true,
		},
		{
			name:      "running task is never reapable",
			hasTask:   true,
			status:    client.Running,
			createdAt: longAgo,
			want:      false,
		},
		{
			name:      "created task is never reapable",
			hasTask:   true,
			status:    client.Created,
			createdAt: longAgo,
			want:      false,
		},
		{
			name:      "paused task is never reapable",
			hasTask:   true,
			status:    client.Paused,
			createdAt: longAgo,
			want:      false,
		},
		{
			name:      "unknown status is never reapable",
			hasTask:   true,
			status:    client.Unknown,
			createdAt: longAgo,
			want:      false,
		},
		{
			name:         "dead but provisioning is never reapable",
			hasTask:      false,
			createdAt:    longAgo,
			provisioning: true,
			want:         false,
		},
		{
			name:      "just-exited within grace is not reapable",
			hasTask:   true,
			status:    client.Stopped,
			exitTime:  justNow,
			createdAt: longAgo,
			want:      false,
		},
		{
			name:      "dead task with no timestamp is skipped",
			hasTask:   false,
			createdAt: time.Time{},
			want:      false,
		},
		{
			name:      "exit time is preferred over old createdAt",
			hasTask:   true,
			status:    client.Stopped,
			exitTime:  justNow, // recent death — keep, even though createdAt is old
			createdAt: longAgo,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := orphanContainerReapable(tt.hasTask, tt.status, tt.exitTime, tt.createdAt, now, grace, tt.provisioning)
			if got != tt.want {
				t.Errorf("orphanContainerReapable() = %v, want %v", got, tt.want)
			}
		})
	}
}
