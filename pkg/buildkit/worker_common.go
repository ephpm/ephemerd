//go:build linux || windows

package buildkit

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/moby/buildkit/worker"
	"github.com/moby/buildkit/worker/base"
)

// newWorkerFromOpt is base.NewWorker, indirected so the unwind contract in
// finishWorkerController is testable.
//
// That contract is only interesting on the FAILURE path, and the real
// base.NewWorker only fails against a live containerd whose metadata is
// corrupt or whose disk is full — neither reproducible in a unit test. The
// property being protected (a failed worker init leaves NO handle under
// DataDir) is exactly the precondition the whole rebuild path depends on, so
// it needs a test more than it needs a pure call graph.
var newWorkerFromOpt = base.NewWorker

// finishWorkerController turns a fully populated base.WorkerOpt into the
// worker.Controller newCore wants, and OWNS the cleanup if it cannot.
//
// OWNERSHIP CONTRACT
//
//   - On success the returned *worker.Controller owns everything: its Close
//     closes each worker, and base.Worker.Close closes MetadataStore (i.e.
//     <DataDir>/worker/metadata_v2.db) and the network providers. `extra`
//     holds resources the OS-specific caller opened that the controller does
//     NOT know about — currently the Windows executor's containerd client —
//     and those keep the pre-existing ownership: they live for as long as the
//     controller does. The caller must not close any of it.
//   - On ANY error return, this function has already closed everything it was
//     handed, newest first, and the caller must not.
//
// WHY THIS EXISTS. containerd.NewWorkerOpt (buildkit@v0.25.1
// worker/containerd/containerd.go:151) opens <DataDir>/worker/metadata_v2.db
// with metadata.NewStore — a bbolt file held under an EXCLUSIVE flock — and
// then returns it inside the WorkerOpt. base.NewWorker can fail AFTER that:
// cache.NewManager's cm.init walks the metadata store and errors on a corrupt
// one, and newSharableMountPool MkdirAll's MountPoolRoot, which fails on a
// full disk. base.NewWorker closes nothing on those paths (worker/base/
// worker.go:118 returns the error bare), and neither did we, so a failed
// worker init leaked the flock for the daemon's lifetime.
//
// That leak is precisely the precondition of the wedge everything else here
// guards against: with a handle held under DataDir, Rebuild's quarantine
// rename cannot succeed on Windows, and every later bbolt open spins forever
// on a flock with a zero timeout. And it fired in the worst possible place —
// a full disk or a corrupt metadata store is *when the repair path runs*, so
// the leak was armed by the very condition it then made unrecoverable.
//
// newCore's own `built []io.Closer` unwind cannot cover this: it only starts
// tracking once newWorkerController has RETURNED, so anything opened and
// dropped inside it is invisible to it.
func finishWorkerController(ctx context.Context, workerOpt base.WorkerOpt, extra []io.Closer, log *slog.Logger) (*worker.Controller, error) {
	// Everything opened before base.NewWorker, newest first. MetadataStore is
	// last because it was opened first (inside NewWorkerOpt).
	unwind := func(reason string) {
		for i := len(extra) - 1; i >= 0; i-- {
			closeQuietly(log, extra[i], reason)
		}
		if workerOpt.MetadataStore != nil {
			closeQuietly(log, workerOpt.MetadataStore, reason)
		}
	}

	w, err := newWorkerFromOpt(ctx, workerOpt)
	if err != nil {
		unwind("new worker failed")
		return nil, fmt.Errorf("new worker: %w", err)
	}

	wc := &worker.Controller{}
	if err := wc.Add(w); err != nil {
		// The worker took ownership of MetadataStore and the network
		// providers, so close IT rather than them — closing both would be a
		// double close of the bbolt handle.
		closeQuietly(log, w, "adding the worker to the controller failed")
		for i := len(extra) - 1; i >= 0; i-- {
			closeQuietly(log, extra[i], "adding the worker to the controller failed")
		}
		return nil, fmt.Errorf("add worker: %w", err)
	}
	return wc, nil
}

// closeQuietly closes c and logs a failure. Every caller is unwinding a
// failure it is already reporting, so there is nowhere to return a second
// error to — but a close that fails means a handle is still held under
// DataDir, and that has to reach the log or the next symptom is an
// unexplained "Access is denied" during a rebuild.
func closeQuietly(log *slog.Logger, c io.Closer, reason string) {
	if c == nil {
		return
	}
	if err := c.Close(); err != nil && log != nil {
		log.Warn("buildkit: unwinding a partially built worker left a handle open under the data dir",
			"reason", reason, "error", err)
	}
}
