package dind

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// mkSymlinkOrSkip creates a symlink, skipping the test when the platform
// forbids it (e.g. Windows without developer mode). The real dind daemon runs
// on Linux where symlinks always work.
func mkSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}
}

// TestTranslateBindSource_SymlinkEscape_Rejected is the F6 regression test: a
// runner that plants a symlink inside its rootfs pointing at the VM host FS
// (e.g. `ln -s / esc`) must not be able to bind-mount the escape target. Before
// the fix, os.Stat followed the link and the resolved path (outside the rootfs)
// was handed straight to containerd.
func TestTranslateBindSource_SymlinkEscape_Rejected(t *testing.T) {
	rootfs := t.TempDir()
	// Somewhere clearly outside the rootfs that we should never be able to bind.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("host-fs"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plant `<rootfs>/esc -> <outside>` — the escape symlink.
	mkSymlinkOrSkip(t, outside, filepath.Join(rootfs, "esc"))

	// A bind for /esc/secret resolves, via the symlink, to <outside>/secret.
	_, err := translateBindSource("/esc/secret", nil, rootfs, "", nil)
	if err == nil {
		t.Fatal("expected rejection of symlink escaping the rootfs, got success")
	}
	if !strings.Contains(err.Error(), "escape") {
		t.Errorf("error %q should explain the rootfs escape", err)
	}
}

// TestTranslateBindSource_SymlinkEscapeViaAncestor_Rejected covers the
// auto-mkdir path: the requested source doesn't exist yet, but an ancestor
// directory is a symlink out of the rootfs. Auto-mkdir must not create (and
// bind) a directory on the VM host FS.
func TestTranslateBindSource_SymlinkEscapeViaAncestor_Rejected(t *testing.T) {
	rootfs := t.TempDir()
	outside := t.TempDir()
	// `<rootfs>/home/runner/_work` is a symlink to <outside>. A bind for
	// /home/runner/_work/_actions (not yet created) would auto-mkdir under
	// <outside> if unchecked.
	if err := os.MkdirAll(filepath.Join(rootfs, "home", "runner"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkSymlinkOrSkip(t, outside, filepath.Join(rootfs, "home", "runner", "_work"))

	_, err := translateBindSource("/home/runner/_work/_actions", nil, rootfs, "", nil)
	if err == nil {
		t.Fatal("expected rejection when an ancestor symlink escapes the rootfs, got success")
	}
	if !strings.Contains(err.Error(), "escape") {
		t.Errorf("error %q should explain the rootfs escape", err)
	}
	// And nothing should have been created under the escape target.
	if _, statErr := os.Stat(filepath.Join(outside, "_actions")); statErr == nil {
		t.Error("auto-mkdir created a directory outside the rootfs via the ancestor symlink")
	}
}

// TestTranslateBindSource_InternalSymlink_Allowed confirms the fix does not
// over-reject: a symlink that stays inside the rootfs (a legitimate overlay
// arrangement) resolves fine.
//
// Both realistic forms are covered, because they are what real runner images
// contain: a relative link, and an ABSOLUTE link written from the container's
// point of view — Ubuntu's merged-usr layout makes /bin, /lib and /sbin
// absolute symlinks into /usr, so every bind that traverses them takes the
// second path. Resolution therefore has to reinterpret an absolute symlink
// relative to the rootfs (which openat2's RESOLVE_IN_ROOT does, and Go's
// os.Root notably does not — it rejects absolute symlinks outright, which is
// why os.Root is not used here).
//
// A link whose target is the rootfs's own HOST path is deliberately not
// covered as an "allowed" case: it cannot occur in a container image (nothing
// inside the container can name the host path of its own rootfs) and treating
// it as in-bounds would mean resolving symlink targets against the host's root
// rather than the container's, which is the escape this all exists to prevent.
// See TestTranslateBindSource_SymlinkEscape_Rejected.
func TestTranslateBindSource_InternalSymlink_Allowed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		target    string // symlink target, as written inside the rootfs
		linuxOnly bool
	}{
		{name: "relative", target: "real"},
		// Reinterpreting an absolute target relative to the rootfs is
		// RESOLVE_IN_ROOT, i.e. a kernel feature. The dev-host stub in
		// bindpin_other.go has no equivalent (it resolves against the real
		// filesystem root) and is not a production path — see the comment
		// there. Running this case off Linux would only assert that the stub
		// is a stub.
		{name: "container_absolute_merged_usr_style", target: "/real", linuxOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.linuxOnly && goruntime.GOOS != "linux" {
				t.Skipf("absolute in-rootfs symlinks are reinterpreted by openat2's RESOLVE_IN_ROOT; goos=%s has no bind translation in production", goruntime.GOOS)
			}
			rootfs := t.TempDir()
			if err := os.MkdirAll(filepath.Join(rootfs, "real", "dir"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rootfs, "real", "dir", "file"), []byte("ok"), 0o644); err != nil {
				t.Fatal(err)
			}
			mkSymlinkOrSkip(t, tc.target, filepath.Join(rootfs, "link"))

			got, err := translateBindSource("/link/dir/file", nil, rootfs, "", nil)
			if err != nil {
				t.Fatalf("in-rootfs symlink should be allowed, got: %v", err)
			}
			if got.ResolvedPath == "" {
				t.Fatal("expected a resolved host path")
			}
			closeBindPins([]*bindPin{got.Pin})
		})
	}
}
