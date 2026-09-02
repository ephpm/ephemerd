//go:build linux || windows

package buildkit

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/worker/base"
)

// TestFinishWorkerController_UnwindsOnWorkerInitFailure is the regression test
// for the leak that armed the original BLOCKER.
//
// containerd.NewWorkerOpt opens <DataDir>/worker/metadata_v2.db — a bbolt file
// under an EXCLUSIVE flock — and returns it inside the WorkerOpt. The next
// step, base.NewWorker, can fail: cache.NewManager's init errors on corrupt
// metadata, and newSharableMountPool's MkdirAll fails on a full disk. Neither
// base.NewWorker nor (before this) newWorkerController closed anything on
// those paths, and newCore's own `built []io.Closer` unwind only starts
// tracking once newWorkerController has RETURNED — so the flock leaked for the
// daemon's lifetime.
//
// A leaked handle under DataDir is the precondition for the unrecoverable
// node: Rebuild's quarantine rename can never succeed on Windows, and every
// later bbolt open spins forever on a zero-timeout flock. Worst of all, the
// leak fires exactly when the repair path is running, because a full disk or a
// corrupt store is *why* it is running.
//
// The probe is the same one the premise test uses: rename the whole DataDir.
// On Windows that is a hard error while any handle inside is open, and it is
// precisely the operation Rebuild performs next.
func TestFinishWorkerController_UnwindsOnWorkerInitFailure(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "buildkit")
	workerRoot := filepath.Join(dataDir, "worker")
	if err := os.MkdirAll(workerRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	// The real thing: the same metadata.NewStore call, on the same path,
	// NewWorkerOpt makes (worker/containerd/containerd.go:151).
	md, err := metadata.NewStore(filepath.Join(workerRoot, "metadata_v2.db"))
	if err != nil {
		t.Fatalf("open worker metadata store: %v", err)
	}

	// Stands in for the Windows path's second containerd client — a resource
	// the worker.Controller never learns about, so only this function can
	// release it.
	extraClosed := 0
	extra := []io.Closer{closerFn(func() error { extraClosed++; return nil })}

	orig := newWorkerFromOpt
	t.Cleanup(func() { newWorkerFromOpt = orig })
	newWorkerFromOpt = func(context.Context, base.WorkerOpt) (*base.Worker, error) {
		// The realistic failure: cache.NewManager -> cm.init on a corrupt
		// metadata store, or newSharableMountPool's MkdirAll on a full disk.
		return nil, errors.New("cache manager: init: metadata store is corrupt")
	}

	wc, err := finishWorkerController(context.Background(),
		base.WorkerOpt{Root: workerRoot, MountPoolRoot: filepath.Join(workerRoot, "cachemounts"), MetadataStore: md},
		extra, discardLog())
	if err == nil {
		t.Fatal("finishWorkerController succeeded although the worker could not be built")
	}
	if wc != nil {
		t.Fatal("a controller was returned alongside an error")
	}
	if extraClosed != 1 {
		t.Errorf("extra closer closed %d times, want 1 — the Windows executor's containerd client leaks on this path", extraClosed)
	}

	// 1. The Windows property: an open handle anywhere under DataDir makes
	//    Rebuild's very next action impossible.
	quarantine := filepath.Join(parent, "quarantined")
	if err := os.Rename(dataDir, quarantine); err != nil {
		t.Fatalf("could not rename the data dir after a failed worker init: %v — metadata_v2.db is still open, which is the precondition of the unrecoverable node", err)
	}

	// 2. The flock property, on every platform. Deadline-guarded because the
	//    failure mode is bolt.Open retrying every 50ms forever, not an error.
	reopened := make(chan error, 1)
	go func() {
		md2, err := metadata.NewStore(filepath.Join(quarantine, "worker", "metadata_v2.db"))
		if err == nil {
			err = md2.Close()
		}
		reopened <- err
	}()
	select {
	case err := <-reopened:
		if err != nil {
			t.Fatalf("reopening metadata_v2.db after a failed worker init: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("reopening metadata_v2.db blocked — the exclusive flock was never released, so every later newCore spins forever holding s.mu")
	}
}

// TestFinishWorkerController_SuccessKeepsOwnership: the unwind must not become
// an over-eager close. On success the worker.Controller owns MetadataStore
// (its Close -> Worker.Close -> MetadataStore.Close) and `extra` stays open
// for the controller's lifetime, exactly as before. Closing either here would
// hand newCore a controller whose store is already shut.
func TestFinishWorkerController_SuccessKeepsOwnership(t *testing.T) {
	parent := t.TempDir()
	workerRoot := filepath.Join(parent, "buildkit", "worker")
	if err := os.MkdirAll(workerRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	md, err := metadata.NewStore(filepath.Join(workerRoot, "metadata_v2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })

	extraClosed := 0
	extra := []io.Closer{closerFn(func() error { extraClosed++; return nil })}

	orig := newWorkerFromOpt
	t.Cleanup(func() { newWorkerFromOpt = orig })
	newWorkerFromOpt = func(context.Context, base.WorkerOpt) (*base.Worker, error) {
		// A zero Worker is enough: worker.Controller.Add only reads its ID
		// and platforms, and nothing here calls through to it.
		return &base.Worker{WorkerOpt: base.WorkerOpt{ID: "test-worker"}}, nil
	}

	wc, err := finishWorkerController(context.Background(),
		base.WorkerOpt{Root: workerRoot, MetadataStore: md}, extra, discardLog())
	if err != nil {
		t.Fatalf("finishWorkerController: %v", err)
	}
	if wc == nil {
		t.Fatal("no controller returned on the success path")
	}
	if extraClosed != 0 {
		t.Errorf("extra closer was closed %d times on the SUCCESS path; the controller owns it from here", extraClosed)
	}
	// Still usable, i.e. not closed behind our back.
	if _, err := md.All(); err != nil {
		t.Errorf("metadata store is unusable after a successful worker init: %v", err)
	}
}
