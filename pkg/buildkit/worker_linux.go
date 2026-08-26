//go:build linux

package buildkit

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/moby/buildkit/executor/oci"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/network/cniprovider"
	"github.com/moby/buildkit/util/network/netproviders"
	"github.com/moby/buildkit/worker"
	"github.com/moby/buildkit/worker/base"
	"github.com/moby/buildkit/worker/containerd"
)

func defaultSnapshotter() string { return "overlayfs" }

// buildkitNetProviderOpt decides how build `RUN` steps are networked.
//
// SECURITY. With BuildKit's default "auto" mode and no CNI config path,
// netproviders.Providers os.Stat("")s a non-existent path and falls back to
// the HOST network namespace. Build steps then run outside every per-container
// netns, so the egress firewall — whose chains only match traffic sourced from
// the container subnet (`-s 10.88.0.0/16`) — never sees them, and a `RUN` step
// can reach the LAN, cloud metadata, and the control plane the firewall exists
// to fence off. Pointing the worker at ephemerd's CNI conflist puts build steps
// on the same bridge/subnet as job containers, so the firewall governs them
// too.
//
// Mode is set to "cni" (not "auto") when a config path is provided so a missing
// or unreadable config fails worker init loudly instead of silently reverting
// to host networking — the exact regression this guards against. When no path
// is provided (e.g. non-dind paths, or platforms wired differently) the prior
// "auto" behaviour is preserved.
//
// Pure so the host-netns-vs-CNI decision is unit-testable without a running
// containerd.
func buildkitNetProviderOpt(cfg Config) netproviders.Opt {
	if cfg.CNIConfigPath == "" {
		return netproviders.Opt{Mode: "auto"}
	}
	return netproviders.Opt{
		Mode: "cni",
		CNI: cniprovider.Opt{
			// The worker keeps its own network-namespace pool state here,
			// separate from the networking Manager's CNI state dir. IPAM
			// (host-local) state is keyed by network name, so allocations stay
			// coordinated across both regardless of this Root.
			Root:       filepath.Join(cfg.DataDir, "cni"),
			ConfigPath: cfg.CNIConfigPath,
			BinaryDir:  cfg.CNIBinDir,
		},
	}
}

// buildkitDNSOpt maps the configured build-container nameservers onto the
// executor's DNS config. Nil (empty input) leaves BuildKit's default — the
// host /etc/resolv.conf — which is only correct when builds run in the host
// netns; once CNIConfigPath moves them onto the isolated bridge the host
// resolver is unreachable and this MUST be set. Split out so the mapping is
// unit-testable without a running containerd. See Config.DNSNameservers.
func buildkitDNSOpt(nameservers []string) *oci.DNSConfig {
	if len(nameservers) == 0 {
		return nil
	}
	return &oci.DNSConfig{Nameservers: nameservers}
}

// newWorkerController constructs a single containerd-backed worker.
// Linux uses overlayfs + runc as the executor.
func newWorkerController(ctx context.Context, cfg Config, _ *session.Manager) (*worker.Controller, error) {
	netOpt := buildkitNetProviderOpt(cfg)

	opts := containerd.WorkerOptions{
		Root:            filepath.Join(cfg.DataDir, "worker"),
		Address:         cfg.ContainerdAddress,
		SnapshotterName: cfg.Snapshotter,
		Namespace:       cfg.ContainerdNamespace,
		NetworkOpt:      netOpt,
		Labels:          map[string]string{"org.ephpm.ephemerd": "true"},
	}
	// Point build-container /etc/resolv.conf at ephemerd's bridge-gateway
	// resolver. Without this the executor inherits the HOST resolv.conf, which
	// a build step on the isolated CNI bridge cannot reach — every `RUN` that
	// resolves a name fails. Must be paired with CNIConfigPath (which moves
	// builds off the host netns in the first place). See Config.DNSNameservers.
	opts.DNS = buildkitDNSOpt(cfg.DNSNameservers)

	workerOpt, err := containerd.NewWorkerOpt(opts)
	if err != nil {
		return nil, fmt.Errorf("containerd worker opt: %w", err)
	}
	// Without this the worker reports an empty GC policy and BuildKit's
	// controller skips collection entirely — the build cache then grows
	// forever. See GCConfig.
	workerOpt.GCPolicy = cfg.GC.PruneInfo()

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
