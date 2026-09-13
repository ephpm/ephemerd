// Package cacheprune reclaims disk held by the daemon's own caches, from
// inside the running daemon.
//
// It exists because `ephemerd cache clear` used to be an offline
// `rm -rf`-style sweep of directories under the data dir, which required
// stopping the daemon and could not distinguish in-use data from garbage.
// Two of those directories cannot be cleared that way at all without
// leaving the node broken or leaking:
//
//   - the BuildKit cache is indexed by BuildKit's own bbolt DB, which holds
//     references to the snapshots backing every cache record. Deleting the
//     containerd image records, leases, or the directory out from under it
//     leaves the snapshots pinned and reclaims nothing (confirmed on a
//     production node: records and leases removed, disk unchanged, until
//     the snapshots were removed too). Pruning has to go through BuildKit.
//   - the containerd store holds images a running job needs.
//
// So the CLI asks the daemon, and the daemon prunes through the manager
// that owns each cache.
package cacheprune

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/ephpm/ephemerd/pkg/buildkit"
	"github.com/ephpm/ephemerd/pkg/dind"
	"github.com/ephpm/ephemerd/pkg/imagegc"
	bkclient "github.com/moby/buildkit/client"
)

// Target names, matching the entries `ephemerd cache list` reports.
const (
	// TargetBuildKit prunes the BuildKit build cache through BuildKit's
	// own cache manager, plus the job-scoped build result records that
	// dead jobs left in the shared buildkit containerd namespace.
	TargetBuildKit = "buildkit"
	// TargetContainerd runs the disk-pressure image collector's eviction
	// over the containerd image store.
	TargetContainerd = "containerd"
)

// AllTargets lists every target this package can prune, in a stable order.
func AllTargets() []string { return []string{TargetBuildKit, TargetContainerd} }

// Result reports the outcome of pruning one target.
type Result struct {
	Name string
	// BytesFreed is the space reclaimed where the underlying manager
	// reports it. Zero means "not reported", not necessarily "nothing" —
	// containerd's image deletion does not return a byte count.
	BytesFreed int64
	// RecordsRemoved counts metadata records dropped.
	RecordsRemoved int64
	// Err is set when this target failed. Other targets still run.
	Err error
}

// Interface is the daemon-side pruner the control server calls. Kept as an
// interface so pkg/scheduler does not have to depend on BuildKit.
type Interface interface {
	Prune(ctx context.Context, targets []string, all bool) []Result
}

// ResolveTargets normalizes a requested target list against the supported
// set. An empty request means "everything". Returns the accepted targets in
// AllTargets order (deduplicated) and any unrecognized names.
//
// Pure — this is the whole request-validation rule, testable without a
// daemon.
func ResolveTargets(requested []string) (accepted, unknown []string) {
	supported := map[string]struct{}{}
	for _, t := range AllTargets() {
		supported[t] = struct{}{}
	}

	if len(requested) == 0 {
		return AllTargets(), nil
	}

	want := map[string]struct{}{}
	for _, r := range requested {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" {
			continue
		}
		if _, ok := supported[r]; !ok {
			unknown = append(unknown, r)
			continue
		}
		want[r] = struct{}{}
	}
	for _, t := range AllTargets() {
		if _, ok := want[t]; ok {
			accepted = append(accepted, t)
		}
	}
	sort.Strings(unknown)
	return accepted, unknown
}

// Pruner implements Interface against a live daemon's managers.
type Pruner struct {
	// Client is the containerd client. Required for the buildkit
	// dead-record sweep and for reporting.
	Client *containerdclient.Client
	// BuildKit is the embedded solver. Nil means dind/buildkit is
	// disabled on this node; the buildkit target then only sweeps dead
	// job records.
	BuildKit *buildkit.Server
	// BuildKitNamespace is the containerd namespace build output lands in.
	BuildKitNamespace string
	// Policy is the configured steady-state GC policy. A non-"all" prune
	// applies it on demand, so an operator-triggered prune and the
	// automatic one collect the same things.
	Policy buildkit.GCConfig
	// ImageGC is the disk-pressure image collector, used for the
	// containerd target. Nil disables that target.
	ImageGC *imagegc.Collector

	Log *slog.Logger
}

// Prune runs each requested target and returns one Result per target. It
// never returns an error for the batch: a failure on one cache must not
// hide the bytes another one freed.
func (p *Pruner) Prune(ctx context.Context, targets []string, all bool) []Result {
	accepted, unknown := ResolveTargets(targets)

	results := make([]Result, 0, len(accepted)+len(unknown))
	for _, u := range unknown {
		results = append(results, Result{Name: u, Err: fmt.Errorf("unknown cache %q", u)})
	}

	for _, t := range accepted {
		switch t {
		case TargetBuildKit:
			results = append(results, p.pruneBuildKit(ctx, all))
		case TargetContainerd:
			results = append(results, p.pruneContainerd(ctx, all))
		}
	}
	return results
}

// pruneBuildKit sweeps dead jobs' build result records and then prunes
// BuildKit's own cache.
//
// all=false applies the configured retention policy, so the warm floor
// survives — the cache exists to keep the next build off the network.
// all=true drops everything not currently in use.
func (p *Pruner) pruneBuildKit(ctx context.Context, all bool) Result {
	res := Result{Name: TargetBuildKit}

	// 1. Job-scoped export records whose job is gone. These are garbage by
	//    construction (the scope name is never reused) and BuildKit's own
	//    GC does not own them.
	if p.Client != nil && p.BuildKitNamespace != "" {
		live, _, err := imagegc.RunningContainers(ctx, p.Client, p.Log)
		if err != nil {
			res.Err = fmt.Errorf("listing running containers: %w", err)
			return res
		}
		n, err := dind.PruneDeadBuildRecords(ctx, p.Client, p.BuildKitNamespace, live, p.Log)
		if err != nil {
			res.Err = fmt.Errorf("pruning dead build records: %w", err)
			return res
		}
		res.RecordsRemoved += int64(n)
	}

	// 2. BuildKit's own cache, through BuildKit.
	if p.BuildKit == nil {
		return res
	}
	rules := []bkclient.PruneInfo{{All: true}}
	if !all {
		rules = p.Policy.PruneInfo()
		if len(rules) == 0 {
			// No policy configured and the caller did not ask for a
			// full drop: nothing well-defined to do. Say so rather
			// than silently wiping a cache the operator wanted kept.
			res.Err = fmt.Errorf("no buildkit GC policy configured; re-run with --all to drop the whole cache")
			return res
		}
	}
	for _, rule := range rules {
		freed, err := p.BuildKit.Prune(ctx, rule)
		res.BytesFreed += freed
		if err != nil {
			res.Err = err
			break
		}
	}
	return res
}

// collectPass names which image-collector entry point a containerd prune
// request maps to. Split out as a pure function so the routing is
// table-testable without a live containerd — the bug it guards against is
// exactly that the request's `all` flag never reached the collector at all.
type collectPass string

const (
	// passWatermark is the ordinary policy pass: evict only what disk
	// pressure justifies.
	passWatermark collectPass = "watermark"
	// passForced is the operator override: evict every unprotected record.
	passForced collectPass = "forced"
)

func containerdPass(all bool) collectPass {
	if all {
		return passForced
	}
	return passWatermark
}

// pruneContainerd runs one image-collector pass.
//
// all=false runs the ordinary watermark-driven pass. Under no disk pressure
// that is a no-op by design: the collector protects images referenced by
// running containers and the node's pinned runner images, and evicting a
// warm image store on demand would only cost a re-pull.
//
// all=true is the operator override. It used to be SILENTLY DROPPED — this
// function took no all parameter at all, so `ephemerd cache clear containerd
// --all` ran the same watermark pass, correctly evicted nothing on a node
// whose thresholds were not tripped, and reported success with zero records
// removed. An operator chasing disk on a Windows node had no way to force
// eviction and no way to tell the flag was being ignored. It now forces the
// policy open (CollectAll) while keeping the safety: live-container images
// and pinned runner images are still an absolute veto.
func (p *Pruner) pruneContainerd(ctx context.Context, all bool) Result {
	res := Result{Name: TargetContainerd}
	pass := containerdPass(all)
	if p.ImageGC == nil {
		// Naming the pass we WOULD have run is not decoration: it is the
		// only externally visible evidence that the request's `all` flag
		// reached this function at all, which is exactly the hop where it
		// used to be dropped silently. It also tells an operator whose
		// `--all` did nothing that the flag was understood and the
		// collector is simply not configured on this node.
		res.Err = fmt.Errorf("image gc is disabled on this node (requested pass: %s)", pass)
		return res
	}
	var gcRes imagegc.Result
	var err error
	switch pass {
	case passForced:
		gcRes, err = p.ImageGC.CollectAll(ctx)
	default:
		gcRes, err = p.ImageGC.Collect(ctx)
	}
	if err != nil {
		res.Err = err
		return res
	}
	res.RecordsRemoved = int64(gcRes.Evicted + gcRes.AgeEvicted)
	if gcRes.After.FreeBytes > gcRes.Before.FreeBytes {
		res.BytesFreed = int64(gcRes.After.FreeBytes - gcRes.Before.FreeBytes)
	}
	return res
}
