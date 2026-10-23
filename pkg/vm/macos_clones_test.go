package vm

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cloneFixture lays out jobs/ the way three VM instances leave it: a disk
// clone, an aux clone and a share dir each, every entry stamped with mtime.
func cloneFixture(t *testing.T, data string, ids map[string]time.Time) string {
	t.Helper()
	jobs := filepath.Join(data, "vm", "macos", "jobs")
	for id, mtime := range ids {
		if err := os.MkdirAll(filepath.Join(jobs, id), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{id + ".img", id + ".aux"} {
			if err := os.WriteFile(filepath.Join(jobs, n), []byte("clone"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for _, n := range []string{id, id + ".img", id + ".aux"} {
			if err := os.Chtimes(filepath.Join(jobs, n), mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
	}
	return jobs
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	return err == nil
}

// TestSweepStaleMacOSClones_LiveAndGrace: the sweep removes only what is
// both unowned AND past the grace period. An old clone that a live VM owns
// stays (a long job's .img can go quiet), and so does a recent unowned one
// (it may belong to a VM registered a moment from now).
func TestSweepStaleMacOSClones_LiveAndGrace(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	data := t.TempDir()
	jobs := cloneFixture(t, data, map[string]time.Time{
		"110392856702-a9e5c1a4": now.Add(-21 * 24 * time.Hour), // the 2026-10-01 leak: unowned, old → removed
		"119992881741-825ce005": now.Add(-5 * time.Hour),       // old but live (long job) → kept
		"120000000001-00000001": now.Add(-30 * time.Minute),    // unowned but recent → kept
	})
	// Things outside jobs/ that must never be touched.
	macos := filepath.Join(data, "vm", "macos")
	for _, n := range []string{"base.img", "aux.bin"} {
		if err := os.WriteFile(filepath.Join(macos, n), []byte("base"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := now.Add(-100 * 24 * time.Hour)
		_ = os.Chtimes(filepath.Join(macos, n), old, old)
	}

	live := map[string]bool{"119992881741-825ce005": true}
	res := SweepStaleMacOSClones(data, live, MacOSCloneGrace, now, discardLog())
	if res.Removed != 3 || len(res.Failed) != 0 {
		t.Fatalf("removed=%d failed=%v, want the leak's 3 entries (img, aux, share) and no failures", res.Removed, res.Failed)
	}
	if res.FreedBytes <= 0 {
		t.Errorf("FreedBytes = %d, want the removed files' size", res.FreedBytes)
	}
	for _, n := range []string{"110392856702-a9e5c1a4", "110392856702-a9e5c1a4.img", "110392856702-a9e5c1a4.aux"} {
		if exists(t, filepath.Join(jobs, n)) {
			t.Errorf("stale unowned %s survived", n)
		}
	}
	for _, id := range []string{"119992881741-825ce005", "120000000001-00000001"} {
		for _, n := range []string{id, id + ".img", id + ".aux"} {
			if !exists(t, filepath.Join(jobs, n)) {
				t.Errorf("%s was swept; it is live or inside the grace period", n)
			}
		}
	}
	for _, n := range []string{"base.img", "aux.bin"} {
		if !exists(t, filepath.Join(macos, n)) {
			t.Errorf("%s outside jobs/ was touched", n)
		}
	}
}

// TestSweepStaleMacOSClones_DoctorMode: no live set and no grace (daemon
// stopped) takes everything in jobs/, and still nothing outside it.
func TestSweepStaleMacOSClones_DoctorMode(t *testing.T) {
	now := time.Now()
	data := t.TempDir()
	cloneFixture(t, data, map[string]time.Time{"a-1": now, "b-2": now.Add(-time.Hour)})
	base := filepath.Join(data, "vm", "macos", "base.img")
	if err := os.WriteFile(base, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := SweepStaleMacOSClones(data, nil, 0, now, discardLog()); res.Removed != 6 {
		t.Errorf("removed %d, want all 6 entries", res.Removed)
	}
	if !exists(t, base) {
		t.Error("base.img was removed")
	}
}

func TestSweepStaleMacOSClones_NoJobsDir(t *testing.T) {
	if res := SweepStaleMacOSClones(t.TempDir(), nil, MacOSCloneGrace, time.Now(), discardLog()); res.Removed != 0 || res.Failed != nil {
		t.Errorf("missing jobs dir: got %+v, want nothing", res)
	}
}

func TestCloneEntryID(t *testing.T) {
	for name, want := range map[string]string{
		"119992881741-825ce005.img": "119992881741-825ce005",
		"119992881741-825ce005.aux": "119992881741-825ce005",
		"119992881741-825ce005":     "119992881741-825ce005",
	} {
		if got := cloneEntryID(name); got != want {
			t.Errorf("cloneEntryID(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestLiveMacOSVMRegistry: an id is protected from registration until its
// teardown unregisters it, and the snapshot is a copy.
func TestLiveMacOSVMRegistry(t *testing.T) {
	const id = "test-live-registry-1"
	registerMacVM(id)
	snap := LiveMacOSVMIDs()
	if !snap[id] {
		t.Fatal("registered id missing from LiveMacOSVMIDs")
	}
	snap["injected"] = true
	if LiveMacOSVMIDs()["injected"] {
		t.Error("LiveMacOSVMIDs returned the registry itself, not a copy")
	}
	unregisterMacVM(id)
	if LiveMacOSVMIDs()[id] {
		t.Error("unregistered id still live")
	}
}

// TestTeardownMacVMFiles_WedgedStopStillRemovesFiles is the leak itself: a
// Virtualization.framework stop that never returns (#196) used to mean the
// clones were never deleted, because deletion came after it. Now they go at
// the bound, and the caller can still wait for the real stop.
func TestTeardownMacVMFiles_WedgedStopStillRemovesFiles(t *testing.T) {
	dir := t.TempDir()
	img, aux, share := filepath.Join(dir, "x.img"), filepath.Join(dir, "x.aux"), filepath.Join(dir, "x")
	for _, p := range []string{img, aux} {
		if err := os.WriteFile(p, []byte("clone"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(share, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	start := time.Now()
	wedged, stopped := teardownMacVMFiles(func() { <-release }, 100*time.Millisecond, []string{img, aux}, []string{share}, discardLog())
	if !wedged {
		t.Error("a stop that never returned was not reported as wedged")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("teardown waited %s for a wedged stop; it must give up at the bound", took)
	}
	for _, p := range []string{img, aux, share} {
		if exists(t, p) {
			t.Errorf("%s survived a wedged stop", p)
		}
	}
	select {
	case <-stopped:
		t.Fatal("stopped closed while the stop was still running")
	default:
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stopped never closed after the stop returned")
	}
}

// TestMacVMStopBound_OutlastsGracefulShutdown: stopGuest gives the guest 15 s
// to shut down before forcing it. Removing the disk inside that window would
// pull it from under a guest that is still flushing, so the bound must clear
// it with room for the force stop.
func TestMacVMStopBound_OutlastsGracefulShutdown(t *testing.T) {
	const gracefulWindow = 15 * time.Second
	if macVMStopBound < gracefulWindow+10*time.Second {
		t.Errorf("macVMStopBound = %s; it must outlast the %s graceful stop plus a force stop", macVMStopBound, gracefulWindow)
	}
}

func TestTeardownMacVMFiles_CleanStop(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "y.img")
	if err := os.WriteFile(img, []byte("clone"), 0o600); err != nil {
		t.Fatal(err)
	}
	ran := false
	// Missing paths and empty strings must be harmless: every exit path
	// calls this, some before the files exist.
	wedged, stopped := teardownMacVMFiles(func() { ran = true }, time.Minute,
		[]string{img, "", filepath.Join(dir, "never-created.aux")}, []string{"", filepath.Join(dir, "no-share")}, discardLog())
	<-stopped
	if wedged || !ran {
		t.Errorf("wedged=%v ran=%v, want a clean stop that ran", wedged, ran)
	}
	if exists(t, img) {
		t.Error("clone survived a clean stop")
	}
}
