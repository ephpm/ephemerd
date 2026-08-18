//go:build linux

package dind

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// requireMountPrivilege skips a test that needs mount(2). The project's Linux
// CI runner is unprivileged; these tests are meant to be run on a Linux node
// (or WSL) as root, and skipping loudly is better than a green run that proved
// nothing — which is exactly how the previous attempt at #125 passed review.
func requireMountPrivilege(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("bind staging needs CAP_SYS_ADMIN (mount(2)); run as root to exercise it")
	}
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestStager builds a real mountStager rooted under dir and guarantees its
// mounts are gone before the test's TempDir cleanup runs.
//
// The ordering is not incidental: t.Cleanup is LIFO, so this registration —
// made after t.TempDir() created its own cleanup — runs first. If it did not,
// os.RemoveAll would walk INTO a live bind mount and delete the files it
// exposes, which in production would be the runner's own rootfs.
func newTestStager(t *testing.T, dataDir, jobID string) *mountStager {
	t.Helper()
	st := newBindStager(dataDir, jobID, discardLog()).(*mountStager)
	t.Cleanup(st.teardown)
	return st
}

// plantVictim builds the shape from the issue's reproduction:
//
//	<dir>/a/real/marker    = PINNED   (what validation sees)
//	<dir>/evil/real/marker = SWAPPED  (what the attacker substitutes)
//
// It returns dir. The "job" later renames a/ away and drops a symlink to evil/
// in its place — the deterministic version of the race.
func plantVictim(t *testing.T, dir string) {
	t.Helper()
	for _, p := range []string{"a/real", "evil/real"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "real", "marker"), []byte("PINNED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "evil", "real", "marker"), []byte("SWAPPED"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// swapVictim performs the attacker's move.
func swapVictim(t *testing.T, dir string) {
	t.Helper()
	if err := os.Rename(filepath.Join(dir, "a"), filepath.Join(dir, "a_real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "evil"), filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
}

// TestBindStaging_SwapAfterValidationDoesNotLeak is the core #125 regression
// test at the kernel level: the staged path must keep pointing at the inode
// that was validated, even after the job swaps every name that led to it.
//
// This is the exact property runc relies on when it walks the spec's bind
// source at task start. TestBindStaging_RealRunc_SwapDoesNotLeak proves runc
// really does honour it; this one runs without a runc binary.
func TestBindStaging_SwapAfterValidationDoesNotLeak(t *testing.T) {
	requireMountPrivilege(t)

	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	plantVictim(t, rootfs)

	stager := newTestStager(t, root, "job-swap")

	// CHECK: resolve and pin, the way translateBindSource does.
	pin, err := pinBindSource(rootfs, "/a/real", true)
	if err != nil {
		t.Fatalf("pinning a contained bind source must succeed: %v", err)
	}
	defer pin.Close()

	staged, err := stager.stage(pin)
	if err != nil {
		t.Fatalf("staging: %v", err)
	}

	// USE: the job swaps the validated directory for a symlink to its own tree.
	swapVictim(t, rootfs)

	// The original path now reads the attacker's content — proving the swap
	// worked and that this test would catch a regression.
	orig, err := os.ReadFile(filepath.Join(rootfs, "a", "real", "marker"))
	if err != nil {
		t.Fatalf("reading the swapped path: %v", err)
	}
	if string(orig) != "SWAPPED" {
		t.Fatalf("swap did not take effect: original path reads %q, want SWAPPED — the test is not exercising the attack", orig)
	}

	// The staged path — what the OCI spec carries — must still be the
	// validated inode.
	got, err := os.ReadFile(filepath.Join(staged, "marker"))
	if err != nil {
		t.Fatalf("reading the staged path %s: %v", staged, err)
	}
	if string(got) != "PINNED" {
		t.Fatalf("TOCTOU: staged bind source %s reads %q after the swap, want PINNED — the container would receive the attacker's target", staged, got)
	}
}

// TestBindStaging_UnstagedPathLeaks is the control. It asserts that the
// pre-fix arrangement — a path string in the spec, re-walked later — really
// does follow the swap. Without this, a passing SwapAfterValidation test
// proves nothing: it could be green because the swap never worked.
func TestBindStaging_UnstagedPathLeaks(t *testing.T) {
	rootfs := t.TempDir()
	plantVictim(t, rootfs)

	// What main returned before this fix: a path string.
	source := filepath.Join(rootfs, "a", "real")

	swapVictim(t, rootfs)

	got, err := os.ReadFile(filepath.Join(source, "marker"))
	if err != nil {
		t.Fatalf("reading the re-walked path: %v", err)
	}
	if string(got) != "SWAPPED" {
		t.Fatalf("re-walking the validated path returned %q, want SWAPPED — this control must reproduce the bug for the staging test above to mean anything", got)
	}
}

// TestBindStaging_LegitimateBindsStillWork is the regression the previous
// attempt shipped: it closed the escape by making every bind fail, legitimate
// ones included. This drives buildBindMounts with the real stager over the
// shapes production actually uses.
func TestBindStaging_LegitimateBindsStillWork(t *testing.T) {
	requireMountPrivilege(t)

	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs")
	// A directory source, a regular-file source, and a directory that does
	// not exist yet (the GHA lazy-bind case Docker auto-creates).
	if err := os.MkdirAll(filepath.Join(rootfs, "home", "runner", "_work", "_temp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "home", "runner", "_work", "_temp", "step.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A merged-usr style absolute symlink inside the rootfs: /bin -> /usr/bin.
	// This is why resolution is openat2/RESOLVE_IN_ROOT and not os.Root, which
	// rejects absolute symlinks outright and would break every Ubuntu runner
	// image.
	if err := os.MkdirAll(filepath.Join(rootfs, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "usr", "bin", "tool"), []byte("tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin", filepath.Join(rootfs, "bin")); err != nil {
		t.Fatal(err)
	}

	// The ephemerd-owned, non-rootfs binds: a socket and two files. These are
	// the passthrough category — no pin, no staging — and they must keep
	// working exactly as before.
	sock := filepath.Join(root, "docker", "d.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatalf("creating a real unix socket for the docker.sock bind: %v", err)
	}
	defer l.Close()
	hosts := filepath.Join(root, "hosts")
	resolv := filepath.Join(root, "resolv.conf")
	for _, p := range []string{hosts, resolv} {
		if err := os.WriteFile(p, []byte("# test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := &Server{
		log:              discardLog(),
		stager:           newTestStager(t, root, "job-legit"),
		runnerRootfsPath: rootfs,
		runnerBindMappings: map[string]string{
			"/var/run/docker.sock": sock,
			"/etc/hosts":           hosts,
			"/etc/resolv.conf":     resolv,
		},
	}

	binds := []string{
		"/var/run/docker.sock:/var/run/docker.sock",
		"/etc/hosts:/etc/hosts",
		"/etc/resolv.conf:/etc/resolv.conf",
		"/home/runner/_work:/__w",
		"/home/runner/_work/_temp:/__w/_temp",
		"/home/runner/_work/_temp/step.sh:/step.sh",
		"/home/runner/_work/_actions:/__w/_actions", // does not exist yet
		"/bin/tool:/tool:ro",                        // through an absolute in-rootfs symlink
	}

	opts, pins, err := s.buildBindMounts(context.Background(), binds)
	t.Cleanup(func() { closeBindPins(pins) })
	if err != nil {
		t.Fatalf("legitimate binds must still work, got: %v", err)
	}
	spec := applyOpts(t, opts)
	if len(spec.Mounts) != len(binds) {
		t.Fatalf("got %d mounts, want %d", len(spec.Mounts), len(binds))
	}

	byDest := map[string]string{}
	for _, m := range spec.Mounts {
		byDest[m.Destination] = m.Source
	}

	// Passthrough sources keep their real paths: nothing job-controlled is in
	// them, and docker.sock is a socket, which is not a valid bind-staging
	// source in the first place.
	for dest, want := range map[string]string{
		"/var/run/docker.sock": sock,
		"/etc/hosts":           hosts,
		"/etc/resolv.conf":     resolv,
	} {
		if byDest[dest] != want {
			t.Errorf("%s source = %q, want the ephemerd-owned path %q", dest, byDest[dest], want)
		}
	}

	// Everything job-supplied is published under the staging dir, and the
	// content behind it is the content that was validated.
	stagingPrefix := jobStagingDir(root, "job-legit") + "/"
	checks := map[string]string{
		"/__w/_temp": "step.sh",
		"/step.sh":   "",
		"/tool":      "",
	}
	for _, dest := range []string{"/__w", "/__w/_temp", "/__w/_actions", "/step.sh", "/tool"} {
		src := byDest[dest]
		if !strings.HasPrefix(src, stagingPrefix) {
			t.Errorf("%s source = %q, want a path under the staging dir %q — a job-supplied source must never reach the spec as its own path", dest, src, stagingPrefix)
			continue
		}
		if _, err := os.Stat(src); err != nil {
			t.Errorf("staged source for %s is not readable: %v", dest, err)
		}
		if child, ok := checks[dest]; ok && child != "" {
			if _, err := os.Stat(filepath.Join(src, child)); err != nil {
				t.Errorf("staged %s does not expose %s: %v", dest, child, err)
			}
		}
	}

	// The auto-created directory must exist in the runner's rootfs (that is
	// the Docker-compat behaviour the GHA runner depends on), not only in the
	// staging dir.
	if _, err := os.Stat(filepath.Join(rootfs, "home", "runner", "_work", "_actions")); err != nil {
		t.Errorf("auto-mkdir did not materialize the lazy bind source in the rootfs: %v", err)
	}

	// The symlink-traversing bind must have resolved to the real file.
	if body, err := os.ReadFile(byDest["/tool"]); err != nil || string(body) != "tool" {
		t.Errorf("bind through the merged-usr symlink read %q (err %v), want %q", body, err, "tool")
	}
}

// TestBindStaging_TeardownUnmountsAndRemoves covers the lifecycle: after the
// job's stager tears down, nothing is left mounted and the directory is gone.
// A leaked staging mount holds a reference to the runner rootfs it was bound
// from, which is what stops containerd from deleting the snapshot.
func TestBindStaging_TeardownUnmountsAndRemoves(t *testing.T) {
	requireMountPrivilege(t)

	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	stager := newTestStager(t, root, "job-teardown")

	pin, err := pinBindSource(rootfs, "/d", false)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := stager.stage(pin)
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	mounts, err := mountPointsUnder(staged)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) == 0 {
		t.Fatalf("no mount at %s after staging — staging did not actually bind anything", staged)
	}

	stager.teardown()

	if mounts, err := mountPointsUnder(jobStagingDir(root, "job-teardown")); err != nil {
		t.Fatal(err)
	} else if len(mounts) != 0 {
		t.Errorf("mounts still present after teardown: %v", mounts)
	}
	if _, err := os.Stat(jobStagingDir(root, "job-teardown")); !os.IsNotExist(err) {
		t.Errorf("staging dir still present after teardown (stat err = %v)", err)
	}
	// Closing the pin afterwards must not blow up or double-unmount.
	if err := pin.Close(); err != nil {
		t.Errorf("closing a pin whose stager already tore down: %v", err)
	}
}

// TestSweepStagedBinds_RemovesLeakedMounts is the startup half of the
// lifecycle. A SIGKILL leaves the mounts behind with no process to release
// them; the next ephemerd must clear them before it tries to reclaim
// snapshots, or the reclaim silently fails on EBUSY.
func TestSweepStagedBinds_RemovesLeakedMounts(t *testing.T) {
	requireMountPrivilege(t)

	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate the previous process: stage, then walk away without teardown.
	leaked := newBindStager(root, "job-killed", discardLog()).(*mountStager)
	t.Cleanup(leaked.teardown) // belt and braces if the sweep under test fails
	pin, err := pinBindSource(rootfs, "/d", false)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := leaked.stage(pin)
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	// pin is deliberately NOT closed: the previous process died holding it.

	SweepStagedBinds(root, discardLog())

	if mounts, err := mountPointsUnder(stagingRootDir(root)); err != nil {
		t.Fatal(err)
	} else if len(mounts) != 0 {
		t.Errorf("sweep left mounts behind: %v", mounts)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("sweep left %s behind (stat err = %v)", staged, err)
	}
}

// TestEnsureTrustedAncestry_RejectsJobWritableParent guards the assumption the
// whole design rests on: runc re-walking the staging path is only safe because
// no component of it can be swapped by anyone but root.
func TestEnsureTrustedAncestry_RejectsJobWritableParent(t *testing.T) {
	requireMountPrivilege(t)

	base := t.TempDir()
	loose := filepath.Join(base, "loose")
	if err := os.MkdirAll(filepath.Join(loose, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	// World-writable with no sticky bit: anyone can rename "child" away.
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ensureTrustedAncestry(filepath.Join(loose, "child")); err == nil {
		t.Fatal("a world-writable, non-sticky ancestor must be rejected")
	}

	// Sticky (the /tmp arrangement) is fine: only the owner can rename or
	// remove entries, so a component cannot be swapped out from under us.
	if err := os.Chmod(loose, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := ensureTrustedAncestry(filepath.Join(loose, "child")); err != nil {
		t.Errorf("a sticky world-writable ancestor is safe and must be accepted, got: %v", err)
	}

	// And the normal case.
	if err := os.Chmod(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureTrustedAncestry(filepath.Join(loose, "child")); err != nil {
		t.Errorf("a root-owned 0700 ancestry must be accepted, got: %v", err)
	}
}

// TestUnescapeMountPath covers the octal escaping the kernel applies to
// mountinfo paths. A staging dir under a data directory with a space in it
// would otherwise be invisible to the sweep, and an invisible mount is one
// that never gets unmounted.
func TestUnescapeMountPath(t *testing.T) {
	cases := map[string]string{
		`/var/lib/ephemerd`:            "/var/lib/ephemerd",
		`/mnt/my\040data/dind-binds`:   "/mnt/my data/dind-binds",
		`/a\011b`:                      "/a\tb",
		`/back\134slash`:               "/back\\slash",
		`/not\09escaped`:               `/not\09escaped`,
	}
	for in, want := range cases {
		if got := unescapeMountPath(in); got != want {
			t.Errorf("unescapeMountPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBindStaging_MountpointTypeMatchesSource makes sure a file source gets a
// file mountpoint. Binding a file onto a directory is ENOTDIR, which would
// break /etc/hosts-shaped binds — a quiet way to regress the feature while all
// the directory tests stay green.
func TestBindStaging_MountpointTypeMatchesSource(t *testing.T) {
	requireMountPrivilege(t)

	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "file"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	stager := newTestStager(t, root, "job-filetype")

	pin, err := pinBindSource(rootfs, "/file", false)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	staged, err := stager.stage(pin)
	if err != nil {
		t.Fatalf("staging a regular file: %v", err)
	}
	var st unix.Stat_t
	if err := unix.Stat(staged, &st); err != nil {
		t.Fatal(err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		t.Errorf("staged mountpoint for a file is mode %o, want a regular file", st.Mode)
	}
	if body, err := os.ReadFile(staged); err != nil || string(body) != "body" {
		t.Errorf("staged file reads %q (err %v), want %q", body, err, "body")
	}
}
