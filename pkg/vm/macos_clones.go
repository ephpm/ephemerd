package vm

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// macOS VM clone lifecycle.
//
// Every macOS job owns three things under <data>/vm/macos/jobs, all named
// after its per-attempt instance id: <id>.img (an APFS clone of base.img),
// <id>.aux (a copy of the aux storage) and <id>/ (the virtio-fs share). The
// .img starts as a free clone and diverges as the guest writes, so a leaked
// one costs whatever the job wrote — the one found on the fleet's Mac on
// 2026-10-22, from a job on 2026-10-01, held 38 GB.
//
// They used to be removed in exactly one place: at the end of Stop, AFTER the
// Virtualization.framework stop returned. That stop is the call #196 showed
// can hang forever, and when it does the scheduler abandons the VM and moves
// on — so the removal never ran, and nothing ever came back for the files. A
// daemon restart or crash mid-job leaked them the same way: the guest dies
// with the process, its files do not.
//
// Three layers now close that:
//
//  1. Start removes the files itself on any error after it created them, so
//     a failed boot leaves nothing even if no one calls Stop.
//  2. Stop removes them unconditionally: the Vz stop runs on its own
//     goroutine with a bound (teardownMacVMFiles), and the files go when it
//     returns OR when the bound expires. Unlinking a disk under a wedged
//     guest is safe — the guest keeps its open descriptor, and the space
//     comes back when the descriptor closes.
//  3. SweepStaleMacOSClones, at startup and periodically, removes anything
//     left in jobs/ that no live VM of this process owns (LiveMacOSVMIDs)
//     and that has not been written for a grace period. That catches what
//     1 and 2 cannot: files from a daemon that died.

// liveMacVMs is the set of macOS VM instance ids this process has created and
// not yet finished tearing down — provisioning, running, or stuck in a
// wedged stop. It is the ground truth the sweep must not touch, and it lives
// here rather than in the scheduler because the scheduler only learns about a
// VM once it is tracked: a VM still provisioning, or one whose teardown was
// abandoned, is invisible to s.running but very much owns its files.
var liveMacVMs = struct {
	sync.Mutex
	ids map[string]struct{}
}{ids: map[string]struct{}{}}

func registerMacVM(id string) {
	liveMacVMs.Lock()
	liveMacVMs.ids[id] = struct{}{}
	liveMacVMs.Unlock()
}

func unregisterMacVM(id string) {
	liveMacVMs.Lock()
	delete(liveMacVMs.ids, id)
	liveMacVMs.Unlock()
}

// LiveMacOSVMIDs returns the instance ids of every macOS VM this process
// still owns files for.
func LiveMacOSVMIDs() map[string]bool {
	liveMacVMs.Lock()
	defer liveMacVMs.Unlock()
	out := make(map[string]bool, len(liveMacVMs.ids))
	for id := range liveMacVMs.ids {
		out[id] = true
	}
	return out
}

// MacOSCloneGrace is how long an unowned entry in jobs/ must have gone
// unwritten before the periodic sweep removes it. The live set is what
// protects running VMs; this is the second guard, for anything the live set
// cannot know about (a second daemon pointed at the same data dir, a clone in
// the instant between apfsCopy and registration). A guest writes to its .img
// continuously, so a live clone never looks two hours old.
const MacOSCloneGrace = 2 * time.Hour

// macVMStopBound is how long Stop waits for Virtualization.framework before
// it removes the VM's files anyway: the 15 s graceful request plus a force
// stop, with headroom. Past it the stop is wedged (#196) and waiting longer
// only means the files never go.
const macVMStopBound = 45 * time.Second

// teardownMacVMFiles runs stopVM on its own goroutine, waits up to bound for
// it, then removes files and dirs regardless of whether it returned. It
// reports whether the stop was still running when the files were removed,
// and returns a channel closed when stopVM finally returns (if ever).
func teardownMacVMFiles(stopVM func(), bound time.Duration, files, dirs []string, log *slog.Logger) (wedged bool, stopped <-chan struct{}) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		stopVM()
	}()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		wedged = true
		log.Warn("macOS VM stop still running after the bound; removing its disk clones anyway",
			"bound", bound, "detail", "the guest keeps its open file until it dies; nothing else would ever delete these")
	}
	removeMacVMFiles(files, dirs, log)
	return wedged, done
}

// removeMacVMFiles deletes a VM's clones and share directory. Idempotent:
// missing paths are not errors, so every exit path can call it.
func removeMacVMFiles(files, dirs []string, log *slog.Logger) {
	for _, p := range files {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Warn("failed to remove VM clone", "path", p, "error", err)
		}
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			log.Warn("failed to remove job directory", "path", d, "error", err)
		}
	}
}

// MacOSCloneSweep is the result of one SweepStaleMacOSClones pass.
type MacOSCloneSweep struct {
	Removed    int
	FreedBytes int64 // allocated bytes, not apparent size: a clone is sparse
	Failed     []string
}

// cloneEntryID maps a jobs/ entry name to the VM instance id that owns it.
func cloneEntryID(name string) string {
	return strings.TrimSuffix(strings.TrimSuffix(name, ".img"), ".aux")
}

// SweepStaleMacOSClones removes entries in <dataDir>/vm/macos/jobs that no
// VM in live owns and that have not been modified for grace. It never looks
// outside jobs/, so base.img, aux.bin and the rest of vm/macos are out of
// reach by construction. grace <= 0 with an empty live set removes
// everything — for `ephemerd doctor`, which only cleans with the daemon
// stopped.
func SweepStaleMacOSClones(dataDir string, live map[string]bool, grace time.Duration, now time.Time, log *slog.Logger) MacOSCloneSweep {
	var res MacOSCloneSweep
	dir := filepath.Join(dataDir, "vm", "macos", "jobs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("cannot list macOS VM clones for the stale sweep", "dir", dir, "error", err)
		}
		return res
	}
	for _, e := range entries {
		if live[cloneEntryID(e.Name())] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if grace > 0 && now.Sub(info.ModTime()) < grace {
			continue
		}
		path := filepath.Join(dir, e.Name())
		size := allocatedBytes(path, info)
		if err := os.RemoveAll(path); err != nil {
			log.Warn("could not remove stale macOS VM clone", "path", path, "error", err)
			res.Failed = append(res.Failed, path)
			continue
		}
		res.Removed++
		res.FreedBytes += size
		log.Info("removed stale macOS VM clone", "path", path,
			"modified", info.ModTime().UTC().Format(time.RFC3339),
			"size_gib", fmt.Sprintf("%.1f", float64(size)/gib))
	}
	return res
}

// allocatedBytes is what removing path gives back. For a file that is its
// allocated blocks where the platform reports them (diskBlocks), because a
// clone's apparent size is the whole virtual disk; for a directory it is the
// sum of its files.
func allocatedBytes(path string, info os.FileInfo) int64 {
	if !info.IsDir() {
		return diskBlocks(info)
	}
	var total int64
	_ = filepath.Walk(path, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			total += diskBlocks(fi)
		}
		return nil
	})
	return total
}
