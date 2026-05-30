package dind

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)


// applyOpts invokes a list of oci.SpecOpts against an empty spec so tests
// can assert what they produced. withBindMount and friends don't touch the
// oci.Client / containers.Container args, so nil values are fine.
func applyOpts(t *testing.T, opts []oci.SpecOpts) *ocispec.Spec {
	t.Helper()
	spec := &ocispec.Spec{}
	for _, opt := range opts {
		if err := opt(context.Background(), nil, nil, spec); err != nil {
			t.Fatalf("apply opt: %v", err)
		}
	}
	return spec
}

func testServer() *Server {
	return &Server{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		mu:  sync.Mutex{},
	}
}

// TestTranslateBindSource_UpperdirMatch_ReturnsReadWrite covers the canonical
// GHA `container:` case: the runner writes /home/runner/_work/_temp/<uuid>.sh
// to its upperdir, then asks dind to mount /home/runner/_work/_temp into a
// sibling container. The sibling must see the file *and* be able to write
// the next step's wrapper script back into the same directory.
func TestTranslateBindSource_UpperdirMatch_ReturnsReadWrite(t *testing.T) {
	upper := t.TempDir()
	if err := os.MkdirAll(filepath.Join(upper, "home", "runner", "_work", "_temp"), 0o755); err != nil {
		t.Fatalf("planting upperdir: %v", err)
	}

	got, err := translateBindSource("/home/runner/_work/_temp", nil, "", upper, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	want := path.Join(upper, "home/runner/_work/_temp")
	if got.HostPath != want {
		t.Errorf("HostPath = %q, want %q", got.HostPath, want)
	}
	if got.ForceReadOnly {
		t.Error("ForceReadOnly = true, want false (upperdir is writable)")
	}
}

// TestTranslateBindSource_LowerdirMatch_ForcesReadOnly guards the security
// property that image-layer files cannot be made writable from a sibling.
// /home/runner/externals lives in the runner image's lowerdir and is shared
// across every container using that base image — a rw mount would corrupt
// the cache for every other job.
func TestTranslateBindSource_LowerdirMatch_ForcesReadOnly(t *testing.T) {
	lower := t.TempDir()
	if err := os.MkdirAll(filepath.Join(lower, "home", "runner", "externals"), 0o755); err != nil {
		t.Fatalf("planting lowerdir: %v", err)
	}

	got, err := translateBindSource("/home/runner/externals", nil, "", t.TempDir(), []string{lower})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	want := path.Join(lower, "home/runner/externals")
	if got.HostPath != want {
		t.Errorf("HostPath = %q, want %q", got.HostPath, want)
	}
	if !got.ForceReadOnly {
		t.Error("ForceReadOnly = false, want true (image layer must stay immutable)")
	}
}

// TestTranslateBindSource_RunnerBind_Translates covers the special case of
// /var/run/docker.sock — that path inside the runner is itself a bind mount
// ephemerd installed (it points at the per-job dind socket file). The
// sibling's -v /var/run/docker.sock:/var/run/docker.sock must redirect to
// the actual socket path on the dind daemon's filesystem.
func TestTranslateBindSource_RunnerBind_Translates(t *testing.T) {
	binds := map[string]string{
		"/var/run/docker.sock": "/run/ephemerd/jobs/abc/docker/d.sock",
	}
	got, err := translateBindSource("/var/run/docker.sock", binds, "", "", nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got.HostPath != "/run/ephemerd/jobs/abc/docker/d.sock" {
		t.Errorf("HostPath = %q, want translated socket path", got.HostPath)
	}
	if got.ForceReadOnly {
		t.Error("ForceReadOnly = true on runner-bind translation; that path category should preserve writability")
	}
}

// TestTranslateBindSource_RunnerBindSubpath_Translates handles sibling
// requests like -v /workspace/foo:/x when the runner has /workspace bound
// to a host scratch dir. The leftover suffix must be appended to the host
// source.
func TestTranslateBindSource_RunnerBindSubpath_Translates(t *testing.T) {
	binds := map[string]string{"/workspace": "/srv/ephemerd/scratch"}
	got, err := translateBindSource("/workspace/foo/bar", binds, "", "", nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	want := "/srv/ephemerd/scratch/foo/bar"
	if got.HostPath != want {
		t.Errorf("HostPath = %q, want %q", got.HostPath, want)
	}
}

// TestTranslateBindSource_LongestPrefixWins guards against a parent bind
// (/etc) shadowing a child bind (/etc/hosts) when both are registered.
// Order in the map is unspecified, so the function has to sort by length.
func TestTranslateBindSource_LongestPrefixWins(t *testing.T) {
	binds := map[string]string{
		"/etc":       "/host/etc",
		"/etc/hosts": "/host/etc/hosts.runtime",
	}
	got, err := translateBindSource("/etc/hosts", binds, "", "", nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got.HostPath != "/host/etc/hosts.runtime" {
		t.Errorf("HostPath = %q — longest prefix /etc/hosts should win over /etc", got.HostPath)
	}
}

// TestTranslateBindSource_NotInRootfs_Rejects asserts the post-fix
// guarantee: bind sources that the dind daemon cannot honor produce an
// error (which surfaces as HTTP 400 at the API layer) instead of silently
// dropping the mount and leaving the workflow to fail with a confusing
// "cannot open" downstream.
func TestTranslateBindSource_NotInRootfs_Rejects(t *testing.T) {
	_, err := translateBindSource("/etc/shadow", nil, "", t.TempDir(), []string{t.TempDir()})
	if err == nil {
		t.Fatal("expected error rejecting unknown bind source, got nil")
	}
}

// TestTranslateBindSource_RelativePath_Rejects keeps the contract simple —
// every source coming from a `docker create -v` is normalized to an
// absolute path by the Docker CLI, so a relative path here is a bug
// somewhere upstream.
func TestTranslateBindSource_RelativePath_Rejects(t *testing.T) {
	_, err := translateBindSource("relative/path", nil, "", t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error on non-absolute source, got nil")
	}
}

// TestTranslateBindSource_DotDotTraversal_StaysInsideUpperdir guards
// against parent traversal escaping upperdir. path.Clean resolves `..`
// within the source path before any join: /home/runner/../foo cleans to
// /home/foo, /.. cleans to /, etc. There is no source path that path.Clean
// turns into anything starting with `..`, so filepath.Join always plants
// the candidate inside the upperdir tree.
func TestTranslateBindSource_DotDotTraversal_StaysInsideUpperdir(t *testing.T) {
	upper := t.TempDir()
	// path.Clean("/home/runner/../home/etc/shadow") = /home/etc/shadow.
	// Plant the file where the cleaned path will land.
	if err := os.MkdirAll(filepath.Join(upper, "home", "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, "home", "etc", "shadow"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := translateBindSource("/home/runner/../etc/shadow", nil, "", upper, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	want := path.Join(upper, "home/etc/shadow")
	if got.HostPath != want {
		t.Errorf("HostPath = %q, want %q (must stay inside upperdir tree)", got.HostPath, want)
	}

	// A path that climbs above /: path.Clean(/../../etc/shadow) = /etc/shadow.
	// Resolution is bounded — even a malicious `..` chain can't escape /.
	// Since we never planted /etc/shadow in upperdir, this should reject.
	if _, err := translateBindSource("/../../../etc/shadow", nil, "", upper, nil); err == nil {
		t.Error("expected rejection of climb-above-root traversal, got success")
	}
}

// TestTranslateBindSource_PreferUpperOverLower asserts that when a path
// exists in both upperdir and a lowerdir (overlay copy-up), the upperdir
// copy wins and the mount stays writable. This is the standard overlayfs
// semantic; the sibling must see the runner's modified version, not the
// pristine image layer.
func TestTranslateBindSource_PreferUpperOverLower(t *testing.T) {
	upper := t.TempDir()
	lower := t.TempDir()
	rel := filepath.Join("home", "runner", "_work", "config")
	if err := os.MkdirAll(filepath.Join(upper, "home", "runner", "_work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(lower, "home", "runner", "_work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, rel), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lower, rel), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := translateBindSource("/"+filepath.ToSlash(rel), nil, "", upper, []string{lower})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	want := path.Join(upper, filepath.ToSlash(rel))
	if got.HostPath != want {
		t.Errorf("HostPath = %q, want upperdir copy %q", got.HostPath, want)
	}
	if got.ForceReadOnly {
		t.Error("ForceReadOnly = true, want false — upperdir copy is writable")
	}
}

// TestTranslateBindSource_RootfsPathResolvesToMergedView is the regression
// test for the /__e/node20 failure mode. When runnerRootfsPath is set,
// sources resolve to "<rootfsPath>/<src>" — a normal directory in the
// host's mount namespace where runc has mounted the runner's merged
// overlay. Every layer's content is visible through that one path, so
// the per-layer walk's "first match wins" pathology (where the first
// lowerdir with the dir entry got picked over a deeper one with the
// actual file) goes away.
//
// We simulate the bundle rootfs with t.TempDir() — same shape as the
// real /run/containerd/.../rootfs path containerd hands us in
// production. No procfs dependency, so the test runs everywhere.
func TestTranslateBindSource_RootfsPathResolvesToMergedView(t *testing.T) {
	rootfs := t.TempDir()
	// Plant the merged view's contents the way overlayfs would expose
	// them through the bundle mount: a directory and a file inside it.
	if err := os.MkdirAll(filepath.Join(rootfs, "home", "runner", "externals", "node20", "bin"), 0o755); err != nil {
		t.Fatalf("plant rootfs: %v", err)
	}
	const body = "#!/bin/sh\necho merged-view\n"
	if err := os.WriteFile(filepath.Join(rootfs, "home", "runner", "externals", "node20", "bin", "node"), []byte(body), 0o755); err != nil {
		t.Fatalf("plant marker: %v", err)
	}

	got, err := translateBindSource("/home/runner/externals/node20/bin/node", nil, rootfs, "", nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	want := path.Join(rootfs, "home/runner/externals/node20/bin/node")
	if got.HostPath != want {
		t.Errorf("HostPath = %q, want %q (must resolve against the rootfs mount)", got.HostPath, want)
	}
	if got.ForceReadOnly {
		t.Error("ForceReadOnly = true on rootfs-path resolution; writes copy-up into the runner's own upperdir and shouldn't be downgraded")
	}
	// Round-trip: reading through the translated path must return the
	// planted bytes, proving the resolved host path actually points at
	// the runner's view of this file.
	gotBody, err := os.ReadFile(got.HostPath)
	if err != nil {
		t.Fatalf("read via translated path: %v", err)
	}
	if string(gotBody) != body {
		t.Errorf("round-trip body = %q, want %q", string(gotBody), body)
	}
}

// TestTranslateBindSource_RootfsPathRejectsMissingSource is the
// loud-failure guard for the rootfs-path resolver. When the rootfs path
// is set and the source isn't under it, translation must error out —
// not fall through to the snapshot-layer walk, which would mask the
// real "runner can't see this either" situation.
func TestTranslateBindSource_RootfsPathRejectsMissingSource(t *testing.T) {
	rootfs := t.TempDir()
	_, err := translateBindSource("/this/path/does/not/exist", nil, rootfs, "", nil)
	if err == nil {
		t.Fatal("expected rejection of missing source under rootfs path, got nil")
	}
	if !strings.Contains(err.Error(), "rootfs") {
		t.Errorf("error should mention rootfs to explain why it was rejected: %v", err)
	}
}

// TestBuildBindMounts_GHARunnerContainer is the canonical regression test
// for the ephpm `container:` failure. The GHA runner inside a job
// container asks dind to create a sibling with the exact bind set the
// upstream runner emits — workspace, _temp, _actions, _tool, externals,
// _github_home, _github_workflow, and the docker socket. Every source
// must translate correctly; the pre-fix shim silently dropped every one
// of them and the sibling exec'd against an empty mountpoint.
func TestBuildBindMounts_GHARunnerContainer(t *testing.T) {
	upper := t.TempDir()
	lower := t.TempDir()
	// Plant the directories the runner creates on startup. _work and
	// children live in upperdir (mutable). externals lives in the image
	// (lowerdir).
	for _, p := range []string{
		"home/runner/_work",
		"home/runner/_work/_temp",
		"home/runner/_work/_actions",
		"home/runner/_work/_tool",
		"home/runner/_work/_temp/_github_home",
		"home/runner/_work/_temp/_github_workflow",
	} {
		if err := os.MkdirAll(filepath.Join(upper, p), 0o755); err != nil {
			t.Fatalf("plant upperdir %s: %v", p, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(lower, "home/runner/externals"), 0o755); err != nil {
		t.Fatalf("plant lowerdir externals: %v", err)
	}

	s := testServer()
	s.runnerSnapshotKey = "runner-snapshot"
	s.runnerBindMappings = map[string]string{
		"/var/run/docker.sock": "/run/ephemerd/jobs/x/docker/d.sock",
	}
	s.rootfsSearchDirsFn = func(_ context.Context, key string) ([]string, error) {
		if key != "runner-snapshot" {
			t.Fatalf("unexpected snapshot key %q", key)
		}
		return []string{upper, lower}, nil
	}

	binds := []string{
		"/var/run/docker.sock:/var/run/docker.sock",
		"/home/runner/_work:/__w",
		"/home/runner/externals:/__e:ro",
		"/home/runner/_work/_temp:/__w/_temp",
		"/home/runner/_work/_actions:/__w/_actions",
		"/home/runner/_work/_tool:/__w/_tool",
		"/home/runner/_work/_temp/_github_home:/github/home",
		"/home/runner/_work/_temp/_github_workflow:/github/workflow",
	}
	opts, err := s.buildBindMounts(context.Background(), binds)
	if err != nil {
		t.Fatalf("buildBindMounts: %v", err)
	}
	spec := applyOpts(t, opts)

	if len(spec.Mounts) != len(binds) {
		t.Fatalf("got %d mounts, want %d", len(spec.Mounts), len(binds))
	}

	// Quick lookup of mounts by destination.
	byDest := map[string]ocispec.Mount{}
	for _, m := range spec.Mounts {
		byDest[m.Destination] = m
	}

	// /var/run/docker.sock must redirect to the per-job socket path —
	// that's how the sibling reaches dind back, and the path inside the
	// runner is itself a bind ephemerd installed.
	if got := byDest["/var/run/docker.sock"].Source; got != "/run/ephemerd/jobs/x/docker/d.sock" {
		t.Errorf("docker.sock source = %q, want translated socket path", got)
	}

	// _temp must land in upperdir and stay rw — the runner writes
	// _temp/<uuid>.sh between docker create and docker exec, and the
	// sibling needs to read it.
	tempMount, ok := byDest["/__w/_temp"]
	if !ok {
		t.Fatal("missing /__w/_temp mount")
	}
	wantTemp := path.Join(upper, "home/runner/_work/_temp")
	if tempMount.Source != wantTemp {
		t.Errorf("_temp source = %q, want %q", tempMount.Source, wantTemp)
	}
	if !containsOpt(tempMount.Options, "rw") {
		t.Errorf("_temp options = %v, want rw (runner writes its wrapper script here)", tempMount.Options)
	}

	// externals is in the image lowerdir; it must be ro (the image is
	// shared across every container using this base — a rw mount could
	// corrupt the cached layer for unrelated jobs).
	extMount, ok := byDest["/__e"]
	if !ok {
		t.Fatal("missing /__e mount")
	}
	wantExt := path.Join(lower, "home/runner/externals")
	if extMount.Source != wantExt {
		t.Errorf("externals source = %q, want %q", extMount.Source, wantExt)
	}
	if !containsOpt(extMount.Options, "ro") {
		t.Errorf("externals options = %v, want ro (image layers must be immutable)", extMount.Options)
	}
}

// TestBuildBindMounts_RejectsUnknownSource is the regression guard for
// the silent-drop bug. Anything the translator can't honor must surface
// as a returned error so the caller can write a 400 — not be quietly
// skipped, which leaves the workflow to fail later with a confusing
// "cannot open" message.
func TestBuildBindMounts_RejectsUnknownSource(t *testing.T) {
	s := testServer()
	s.runnerSnapshotKey = "runner-snapshot"
	s.rootfsSearchDirsFn = func(_ context.Context, _ string) ([]string, error) {
		return []string{t.TempDir()}, nil
	}

	_, err := s.buildBindMounts(context.Background(), []string{"/etc/shadow:/x"})
	if err == nil {
		t.Fatal("expected error rejecting unknown bind source, got nil")
	}
	if !strings.Contains(err.Error(), "/etc/shadow") {
		t.Errorf("error %q should name the offending source", err)
	}
}

// TestBuildBindMounts_NoRunnerRegistered keeps the failure mode clear
// when SetRunnerRootfs hasn't been called yet (shouldn't happen in
// production but worth guarding). With no rootfs, any source that isn't
// already in the runner-bind table gets rejected loudly.
func TestBuildBindMounts_NoRunnerRegistered(t *testing.T) {
	s := testServer()
	_, err := s.buildBindMounts(context.Background(), []string{"/home/runner/_work/_temp:/x"})
	if err == nil {
		t.Fatal("expected error when runner rootfs is unregistered, got nil")
	}
}

func containsOpt(opts []string, want string) bool {
	for _, o := range opts {
		if o == want {
			return true
		}
	}
	return false
}
