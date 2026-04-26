//go:build linux

package buildkit

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/network/netproviders"
	"github.com/moby/buildkit/worker"
	"github.com/moby/buildkit/worker/base"
	"github.com/moby/buildkit/worker/containerd"
)

func defaultSnapshotter() string { return "overlayfs" }

// newWorkerController constructs a single containerd-backed worker.
// Linux uses overlayfs + runc as the executor.
func newWorkerController(ctx context.Context, cfg Config, _ *session.Manager) (*worker.Controller, error) {
	netOpt := netproviders.Opt{Mode: "auto"}

	opts := containerd.WorkerOptions{
		Root:            filepath.Join(cfg.DataDir, "worker"),
		Address:         cfg.ContainerdAddress,
		SnapshotterName: cfg.Snapshotter,
		Namespace:       cfg.ContainerdNamespace,
		NetworkOpt:      netOpt,
		Labels:          map[string]string{"org.ephpm.ephemerd": "true"},
	}

	workerOpt, err := containerd.NewWorkerOpt(opts)
	if err != nil {
		return nil, fmt.Errorf("containerd worker opt: %w", err)
	}

	w, err := base.NewWorker(ctx, workerOpt)
	if err != nil {
		return nil, fmt.Errorf("new worker: %w", err)
	}

	wc := &worker.Controller{}
	if err := wc.Add(w); err != nil {
		return nil, fmt.Errorf("add worker: %w", err)
	}
	return wc, nil
}
