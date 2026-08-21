//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// TestRunnerOverlay_JobCannotPoisonSharedTree is the on-metal F1 proof. It
// realizes the exact overlay mount prepareJobRunnerTree asks runc to make,
// then does what the attack does — an in-place append to /actions-runner/run.sh
// from inside a job's view — and asserts the shared, persistent runner tree and
// a second concurrent job are both unaffected.
//
// On current main this containment does not exist: the per-job tree is a
// `cp -al` hardlink farm bind-mounted rw, so the append rewrites the shared
// inode and this test's assertions fail. It requires root (mount(2)) and a
// filesystem that supports overlayfs, which every fleet host has (containerd
// runs the overlayfs snapshotter); it skips otherwise.
func TestRunnerOverlay_JobCannotPoisonSharedTree(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("overlay mount requires root")
	}

	// Shared, persistent runner tree — the thing that must stay immutable.
	shared := makeSharedRunnerTree(t)
	const originalRunSh = "#!/bin/sh\n# ORIGINAL\n"

	// A single filesystem hosting all per-job layers, matching production
	// where upper+work live under <data>/runners next to the shared tree.
	parent := t.TempDir()

	mountJob := func(id string) string {
		jobDir := filepath.Join(parent, "job-"+id)
		_, opt, err := prepareJobRunnerTree(shared, jobDir, "/actions-runner")
		if err != nil {
			t.Fatalf("prepareJobRunnerTree(%s): %v", id, err)
		}
		s := &ocispec.Spec{}
		if err := opt(nil, nil, nil, s); err != nil {
			t.Fatalf("apply opt(%s): %v", id, err)
		}
		if len(s.Mounts) != 1 || s.Mounts[0].Type != "overlay" {
			t.Fatalf("job %s: expected one overlay mount, got %+v", id, s.Mounts)
		}
		merged := filepath.Join(jobDir, "merged")
		if err := os.MkdirAll(merged, 0o755); err != nil {
			t.Fatal(err)
		}
		data := strings.Join(s.Mounts[0].Options, ",")
		if err := syscall.Mount("overlay", merged, "overlay", 0, data); err != nil {
			t.Skipf("overlayfs mount unsupported here (%v); cannot run the on-metal F1 proof", err)
		}
		t.Cleanup(func() { _ = syscall.Unmount(merged, 0) })
		return merged
	}

	jobA := mountJob("A")
	jobB := mountJob("B")

	// The attack: an in-place append from job A's view of the runner tree.
	runShA := filepath.Join(jobA, "run.sh")
	f, err := os.OpenFile(runShA, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open job A run.sh: %v", err)
	}
	if _, err := f.WriteString("# POISONED\n"); err != nil {
		t.Fatalf("append job A run.sh: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Job A's own view sees the write (the overlay is genuinely writable).
	if got, _ := os.ReadFile(runShA); !strings.Contains(string(got), "POISONED") {
		t.Errorf("job A did not observe its own write; overlay is not writable: %q", string(got))
	}

	// The shared tree must be byte-for-byte unchanged.
	if got, err := os.ReadFile(filepath.Join(shared, "run.sh")); err != nil {
		t.Fatal(err)
	} else if string(got) != originalRunSh {
		t.Errorf("SHARED runner tree was poisoned by job A: run.sh = %q, want %q", string(got), originalRunSh)
	}

	// A second concurrent job must not see job A's write.
	if got, err := os.ReadFile(filepath.Join(jobB, "run.sh")); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(got), "POISONED") {
		t.Errorf("job B saw job A's write — cross-job poisoning (F1): %q", string(got))
	}
}
