//go:build !darwin

package dind

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// TestBindTranslation_RealContainerd is the high-fidelity check that the
// translation actually pairs with a live overlayfs snapshotter. It does NOT
// stub rootfsSearchDirsFn — every layer path is resolved through the real
// snapshot service.
//
// The flow mirrors what happens in production for a GHA `container:`
// workflow on a self-hosted ephemerd runner:
//
//  1. The runtime creates a runner container with an overlay snapshot.
//     Here we Prepare the snapshot directly via the snapshotter API and
//     stage a marker file into its upperdir, which is the same place the
//     real GHA runner writes /home/runner/_work/_temp/<uuid>.sh.
//  2. SetRunnerRootfs registers the snapshot key with dind.
//  3. The shim is asked to translate the exact bind shape the upstream
//     GHA runner emits when handling `container:`.
//  4. The translation has to find the marker via the snapshot's upperdir
//     and produce a real on-disk source path.
//
// Without the translation fix this test fails: the legacy
// `os.Stat`-skip path drops every bind whose source isn't on the dind
// daemon's filesystem, and buildBindMounts now returns an error (the
// loud-failure replacement) so the test gets a clear "source not visible"
// instead of the original silent-drop confusion.
func TestBindTranslation_RealContainerd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping bind-translation e2e in short mode")
	}
	if goruntime.GOOS != "linux" {
		// The fix is Linux-only (overlayfs snapshotter). Windows-native
		// jobs use a different snapshotter and bind model — see arch
		// doc, deferred follow-ups.
		t.Skipf("bind translation requires overlayfs snapshotter; goos=%s", goruntime.GOOS)
	}

	ctrdClient := sharedTestContainerd(t)
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// The runner snapshot lives in the runtime's "ephemerd" namespace —
	// runnerRootfsLayers explicitly switches namespaces before consulting
	// the snapshotter, so the e2e has to stage it there too.
	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), sharedNamespace), 60*time.Second)
	defer cancel()

	// Hold a lease so containerd's GC doesn't reap our scratch snapshot
	// mid-test. Same pattern as registry_e2e.
	lease, err := ctrdClient.LeasesService().Create(ctx, leases.WithExpiration(5*time.Minute))
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	t.Cleanup(func() {
		ctx := namespaces.WithNamespace(context.Background(), sharedNamespace)
		if err := ctrdClient.LeasesService().Delete(ctx, lease); err != nil {
			t.Logf("delete lease: %v", err)
		}
	})
	ctx = leases.WithLease(ctx, lease.ID)

	snapshotter := ctrdClient.SnapshotService("overlayfs")
	if snapshotter == nil {
		t.Skip("overlayfs snapshotter unavailable in this containerd build")
	}

	const (
		parentPrepKey = "bind-translate-e2e-parent-prep"
		parentKey     = "bind-translate-e2e-parent"
		snapshotKey   = "bind-translate-e2e"
	)
	t.Cleanup(func() {
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCleanup()
		cleanup = namespaces.WithNamespace(cleanup, sharedNamespace)
		for _, k := range []string{snapshotKey, parentKey, parentPrepKey} {
			if err := snapshotter.Remove(cleanup, k); err != nil && !strings.Contains(err.Error(), "not found") {
				t.Logf("snapshot cleanup %s: %v", k, err)
			}
		}
	})

	// Stage an empty committed parent so the active snapshot on top is
	// a real overlay mount with upperdir= / lowerdir= options. A direct
	// Prepare(..., "") returns a plain bind to the snapshot fs dir,
	// which has no overlayfs layout for translation to find.
	if _, err := snapshotter.Prepare(ctx, parentPrepKey, ""); err != nil {
		t.Fatalf("prepare parent: %v", err)
	}
	if err := snapshotter.Commit(ctx, parentKey, parentPrepKey); err != nil {
		t.Fatalf("commit parent: %v", err)
	}

	mounts, err := snapshotter.Prepare(ctx, snapshotKey, parentKey)
	if err != nil {
		t.Fatalf("snapshotter.Prepare: %v", err)
	}
	upperdir := extractUpperdir(t, mounts)
	if upperdir == "" {
		t.Fatalf("no upperdir in snapshot mounts: %+v", mounts)
	}

	// Plant the directory the GHA runner creates on startup, and a marker
	// file inside it standing in for the step's wrapper script. Translation
	// will resolve `/home/runner/_work/_temp` against this upperdir and
	// produce a bind source that points at this exact file.
	if err := os.MkdirAll(filepath.Join(upperdir, "home", "runner", "_work", "_temp"), 0o755); err != nil {
		t.Fatalf("plant _temp dir: %v", err)
	}
	markerPath := filepath.Join(upperdir, "home", "runner", "_work", "_temp", "marker.sh")
	const markerBody = "#!/bin/sh\necho hello-from-runner-upperdir\n"
	if err := os.WriteFile(markerPath, []byte(markerBody), 0o755); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// Bring up dind backed by the same containerd. The job namespace is
	// distinct from the runner namespace — that's the same separation
	// production uses.
	dataDir, err := os.MkdirTemp("", "ephemerd-bind-e2e-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dataDir); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})

	s, err := New(Config{
		JobID:   "bind-translate-e2e",
		DataDir: dataDir,
		Client:  ctrdClient,
		Log:     log,
	})
	if err != nil {
		t.Fatalf("dind New: %v", err)
	}

	// Wire the runner snapshot the way the runtime does. The bind mappings
	// cover the non-rootfs paths ephemerd installs into the runner — we
	// only need docker.sock for this test, but supply the rest to match
	// the production shape.
	socketPath := filepath.Join(dataDir, "jobs", "bind-translate-e2e", "docker", "d.sock")
	bindMappings := map[string]string{
		"/var/run/docker.sock": socketPath,
		"/etc/hosts":           filepath.Join(dataDir, "hosts", "fake.hosts"),
		"/etc/resolv.conf":     filepath.Join(dataDir, "dns", "fake.conf"),
	}
	s.SetRunnerRootfs(snapshotKey, bindMappings)

	// Drive buildBindMounts with the exact bind set the upstream GHA
	// runner emits for `container:` workflows (verbatim from the ephpm
	// failure log). Translation must succeed for every entry — pre-fix
	// behavior silently dropped them all.
	binds := []string{
		"/var/run/docker.sock:/var/run/docker.sock",
		"/home/runner/_work:/__w",
		"/home/runner/_work/_temp:/__w/_temp",
		"/home/runner/_work/_actions:/__w/_actions",
		"/home/runner/_work/_tool:/__w/_tool",
		"/home/runner/_work/_temp/_github_home:/github/home",
		"/home/runner/_work/_temp/_github_workflow:/github/workflow",
	}
	// Pre-plant the directories the runner creates so translation can
	// resolve them in upperdir. We only planted _temp above; the rest are
	// siblings the runner makes at startup.
	for _, p := range []string{
		"home/runner/_work",
		"home/runner/_work/_actions",
		"home/runner/_work/_tool",
		"home/runner/_work/_temp/_github_home",
		"home/runner/_work/_temp/_github_workflow",
	} {
		if err := os.MkdirAll(filepath.Join(upperdir, p), 0o755); err != nil {
			t.Fatalf("plant %s: %v", p, err)
		}
	}

	opts, err := s.buildBindMounts(ctx, binds)
	if err != nil {
		t.Fatalf("buildBindMounts: %v", err)
	}
	spec := applyOpts(t, opts)

	if len(spec.Mounts) != len(binds) {
		t.Fatalf("got %d mounts, want %d", len(spec.Mounts), len(binds))
	}

	byDest := map[string]string{}
	for _, m := range spec.Mounts {
		byDest[m.Destination] = m.Source
	}

	// docker.sock should route to the per-job socket path the bind
	// mappings registered, not to anything inside the snapshot.
	if got := byDest["/var/run/docker.sock"]; got != socketPath {
		t.Errorf("docker.sock translated to %q, want %q", got, socketPath)
	}

	// _temp must resolve into the snapshot upperdir, and the marker file
	// must be reachable from that path — proves the snapshot's actual
	// on-disk layout is what translation hands to containerd.
	tempSrc := byDest["/__w/_temp"]
	wantTempPrefix := filepath.Join(upperdir, "home", "runner", "_work", "_temp")
	if !strings.HasPrefix(filepath.Clean(tempSrc), filepath.Clean(wantTempPrefix)) {
		t.Errorf("_temp source %q does not point into upperdir %q", tempSrc, wantTempPrefix)
	}
	gotMarker, err := os.ReadFile(filepath.Join(tempSrc, "marker.sh"))
	if err != nil {
		t.Fatalf("read marker through translated bind source: %v", err)
	}
	if string(gotMarker) != markerBody {
		t.Errorf("marker round-trip: got %q, want %q", string(gotMarker), markerBody)
	}
}

// TestBindTranslation_RejectsForeignSource is the loud-failure regression
// guard. Against today's main this would *pass silently* — the legacy code
// drops the bind and continues, leaving the test no way to notice. Against
// the fix, `/etc/shadow` is not in the runner rootfs or bind table, so
// buildBindMounts returns an error that the handler will surface as 400.
func TestBindTranslation_RejectsForeignSource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping bind-translation e2e in short mode")
	}
	if goruntime.GOOS != "linux" {
		t.Skipf("bind translation requires overlayfs snapshotter; goos=%s", goruntime.GOOS)
	}

	ctrdClient := sharedTestContainerd(t)
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), sharedNamespace), 30*time.Second)
	defer cancel()
	lease, err := ctrdClient.LeasesService().Create(ctx, leases.WithExpiration(2*time.Minute))
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	t.Cleanup(func() {
		ctx := namespaces.WithNamespace(context.Background(), sharedNamespace)
		if err := ctrdClient.LeasesService().Delete(ctx, lease); err != nil {
			t.Logf("delete lease: %v", err)
		}
	})
	ctx = leases.WithLease(ctx, lease.ID)

	snapshotter := ctrdClient.SnapshotService("overlayfs")
	if snapshotter == nil {
		t.Skip("overlayfs snapshotter unavailable in this containerd build")
	}
	// The rejection path doesn't need an overlay mount — buildBindMounts
	// fails out at the bind-table / rootfs lookup before any layer walk
	// matters. A bare snapshot is enough; we only need the key to be
	// registered so SetRunnerRootfs takes the on-path branch.
	const snapshotKey = "bind-translate-reject-e2e"
	t.Cleanup(func() {
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCleanup()
		cleanup = namespaces.WithNamespace(cleanup, sharedNamespace)
		if err := snapshotter.Remove(cleanup, snapshotKey); err != nil && !strings.Contains(err.Error(), "not found") {
			t.Logf("snapshot cleanup: %v", err)
		}
	})
	if _, err := snapshotter.Prepare(ctx, snapshotKey, ""); err != nil {
		t.Fatalf("snapshotter.Prepare: %v", err)
	}

	dataDir, err := os.MkdirTemp("", "ephemerd-bind-reject-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })

	s, err := New(Config{
		JobID:   "bind-reject-e2e",
		DataDir: dataDir,
		Client:  ctrdClient,
		Log:     log,
	})
	if err != nil {
		t.Fatalf("dind New: %v", err)
	}
	s.SetRunnerRootfs(snapshotKey, nil)

	_, err = s.buildBindMounts(ctx, []string{"/etc/shadow:/x"})
	if err == nil {
		t.Fatal("expected error rejecting /etc/shadow, got nil — silent-drop regression")
	}
	if !strings.Contains(err.Error(), "/etc/shadow") {
		t.Errorf("error %q should name the offending source", err)
	}
}

// extractUpperdir pulls the upperdir path out of containerd's overlayfs
// mount options. Format: comma-separated "upperdir=<path>" within Options.
func extractUpperdir(t *testing.T, mounts []mount.Mount) string {
	t.Helper()
	for _, m := range mounts {
		for _, opt := range m.Options {
			for _, part := range strings.Split(opt, ",") {
				if strings.HasPrefix(part, "upperdir=") {
					return strings.TrimPrefix(part, "upperdir=")
				}
			}
		}
	}
	return ""
}
