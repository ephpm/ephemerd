package main

import (
	"testing"

	"github.com/ephpm/ephemerd/pkg/cacheprune"
)

// TestDaemonPrunable pins which caches route to the running daemon rather
// than to the filesystem sweep. Getting this wrong on "buildkit" is not a
// cosmetic bug: deleting that directory behind BuildKit's back leaves the
// snapshots pinned by its bbolt index, so the sweep reports bytes freed and
// the disk does not move.
func TestDaemonPrunable(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"buildkit", true},
		{"containerd", true},
		{"images", false},
		{"jobs", false},
		{"runners", false},
		{"vm", false},
		{"worker", false},
		{"", false},
		{"BuildKit", false}, // entry names are lowercase; no fuzzy matching here
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonPrunable(tc.name); got != tc.want {
				t.Errorf("daemonPrunable(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestSplitDaemonPrunable(t *testing.T) {
	entries := managedCaches()
	viaDaemon, viaFS := splitDaemonPrunable(entries)

	if len(viaDaemon)+len(viaFS) != len(entries) {
		t.Fatalf("split lost entries: %d + %d != %d", len(viaDaemon), len(viaFS), len(entries))
	}
	for _, c := range viaDaemon {
		if !daemonPrunable(c.Name) {
			t.Errorf("%q routed to the daemon but is not daemon-prunable", c.Name)
		}
	}
	for _, c := range viaFS {
		if daemonPrunable(c.Name) {
			t.Errorf("%q routed to the filesystem but the daemon can prune it", c.Name)
		}
	}

	// Every target the pruner supports should correspond to a real cache
	// entry, or `cache clear <name>` would reject a name the daemon
	// understands.
	for _, target := range cacheprune.AllTargets() {
		if _, ok := cacheByName(target); !ok {
			t.Errorf("pruner target %q has no matching cache entry in `cache list`", target)
		}
	}

	// Empty input must not panic or invent entries.
	d, f := splitDaemonPrunable(nil)
	if len(d) != 0 || len(f) != 0 {
		t.Errorf("splitDaemonPrunable(nil) = %v, %v; want empty", d, f)
	}
}
