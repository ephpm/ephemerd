//go:build windows

package buildkit

import (
	"context"
	"fmt"
	"path/filepath"

	ctd "github.com/containerd/containerd/v2/client"
	"github.com/moby/buildkit/executor/containerdexecutor"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/network"
	"github.com/moby/buildkit/util/network/netproviders"
	"github.com/moby/buildkit/worker"
	"github.com/moby/buildkit/worker/base"
	"github.com/moby/buildkit/worker/containerd"
)

func defaultSnapshotter() string { return "windows" }

// newWorkerController constructs a single containerd-backed worker.
//
// Two non-default Windows tweaks live here:
//
//  1. Runtime Options is left nil so runhcs uses its default (process
//     isolation). Forcing Hyper-V isolation requires populating
//     SandboxImage and SandboxPlatform; the shim rejects an empty platform
//     with "invalid runtime sandbox platform". Build steps run in the host
//     kernel — acceptable here because the inputs are internal Dockerfiles.
//
//  2. The default network providers are replaced with our HCN-backed
//     provider when cfg.Network is set. BuildKit's stock Windows fallback
//     is NoneProvider, which leaves build containers with no NetworkAdapter
//     and breaks every RUN step that hits the network. We rebuild the
//     executor + WorkerOpt.NetworkProviders so the build container gets a
//     fresh HCN NAT endpoint per task.
func newWorkerController(ctx context.Context, cfg Config, _ *session.Manager) (*worker.Controller, error) {
	netOpt := netproviders.Opt{Mode: "auto"}

	runtime := &containerdexecutor.RuntimeInfo{
		Name: "io.containerd.runhcs.v1",
	}

	opts := containerd.WorkerOptions{
		Root:            filepath.Join(cfg.DataDir, "worker"),
		Address:         cfg.ContainerdAddress,
		SnapshotterName: cfg.Snapshotter,
		Namespace:       cfg.ContainerdNamespace,
		NetworkOpt:      netOpt,
		Labels:          map[string]string{"org.ephpm.ephemerd": "true"},
		Runtime:         runtime,
	}

	workerOpt, err := containerd.NewWorkerOpt(opts)
	if err != nil {
		return nil, fmt.Errorf("containerd worker opt: %w", err)
	}

	if cfg.Network != nil {
		hcnProvider := newHCNNetworkProvider(cfg.Network, cfg.Log)
		np := map[pb.NetMode]network.Provider{
			pb.NetMode_UNSET: hcnProvider,
			pb.NetMode_NONE:  network.NewNoneProvider(),
		}

		// Re-build the executor with the same options NewWorkerOpt used,
		// substituting our network providers. We need a fresh client; the
		// one inside NewWorkerOpt isn't exposed, but a second client to the
		// same containerd is harmless (gRPC connections are independent).
		client, err := ctd.New(opts.Address, ctd.WithDefaultNamespace(opts.Namespace))
		if err != nil {
			return nil, fmt.Errorf("buildkit hcn executor client: %w", err)
		}
		executorOpts := containerdexecutor.ExecutorOptions{
			Client:           client,
			Root:             workerOpt.Root,
			Runtime:          runtime,
			NetworkProviders: np,
			// Hyper-V isolation is required for the build container's HCN
			// endpoint to give it real DNS. Process-isolated containers
			// don't pick up HCN endpoint DNS the way Hyper-V (UVM-backed)
			// ones do — go.dev fails to resolve. The executor toggles this
			// to add Windows.HyperV to the OCI spec; runhcs reads
			// Windows.HyperV and creates a UVM around the container.
			HyperVIsolation: true,
		}
		workerOpt.Executor = containerdexecutor.New(executorOpts)
		workerOpt.NetworkProviders = np
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
