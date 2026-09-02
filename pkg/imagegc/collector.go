package imagegc

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/ephpm/ephemerd/pkg/diskspace"
)

// DefaultExhaustedBackoff is how long the collector stays quiet after a pass
// that evicted everything it was allowed to and still could not get under
// the high watermark. See Collector.Collect's failsafe.
const DefaultExhaustedBackoff = 30 * time.Minute

// Config constructs a Collector.
type Config struct {
	// Client is the containerd client whose namespaces are collected.
	Client *client.Client

	// Path is any path on the filesystem to measure — in practice the
	// containerd root under the data dir. Capacity is read with one
	// statfs-class syscall; nothing walks the tree.
	Path string

	// Thresholds are the pressure watermarks. A zero Thresholds disables
	// pressure-triggered collection (the age backstop, if configured,
	// still runs).
	Thresholds Thresholds

	// Namespaces are collected by exact name (e.g. "ephemerd").
	Namespaces []string

	// NamespacePrefixes are collected by prefix (e.g.
	// "ephemerd-dind-cache-").
	NamespacePrefixes []string

	// PinnedImages are refs that must never be evicted regardless of age
	// — the node's configured runner images. Evicting one forces an
	// immediate re-pull on the next job.
	PinnedImages []string

	// LiveJobPrefixes maps a live container ID to image-name prefixes
	// whose records belong to that job and must survive the pass.
	//
	// Needed for the shared "buildkit" namespace: a job's `docker build`
	// output is stored there under a name scoped by the job's container
	// ID, and no container references it, so nothing else marks it live.
	// Nil disables the expansion.
	LiveJobPrefixes func(containerID string) []string

	// MaxAge is the OPTIONAL age backstop. Zero (the default) disables
	// it. When set, records idle longer than this are evicted on every
	// pass whether or not the disk is under pressure. This is not the
	// primary mechanism — see the package doc.
	MaxAge time.Duration

	// ExhaustedBackoff is how long to skip passes after the failsafe
	// trips. Zero uses DefaultExhaustedBackoff.
	ExhaustedBackoff time.Duration

	Log *slog.Logger
}

// Collector evicts image records under disk pressure.
//
// A Collector serializes its own passes: the periodic timer and the
// pre-pull headroom check share one mutex, so a job that starts mid-sweep
// waits for the sweep instead of racing it.
//
// The nil Collector is a valid no-op, so callers that may not have GC
// configured can invoke it unconditionally.
type Collector struct {
	cfg Config

	mu sync.Mutex
	// exhaustedUntil suppresses passes after the failsafe trips. Guarded
	// by mu.
	exhaustedUntil time.Time

	// now is time.Now, swappable in tests.
	now func() time.Time
}

// New builds a Collector. Returns nil when no mechanism is configured, so
// the caller's nil check doubles as the "is GC on" check.
func New(cfg Config) *Collector {
	if cfg.Client == nil || cfg.Path == "" {
		return nil
	}
	if !cfg.Thresholds.Enabled() && cfg.MaxAge <= 0 {
		return nil
	}
	if cfg.ExhaustedBackoff <= 0 {
		cfg.ExhaustedBackoff = DefaultExhaustedBackoff
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	cfg.Log = cfg.Log.With("component", "image-gc")
	return &Collector{cfg: cfg, now: time.Now}
}

// Result summarizes one pass.
type Result struct {
	// Before and After are the capacity readings that bracket the pass.
	Before, After diskspace.Usage
	// Pressure is the verdict on Before.
	Pressure Pressure
	// AgeEvicted counts records dropped by the optional age backstop.
	AgeEvicted int
	// Evicted counts records dropped by the pressure pass.
	Evicted int
	// Protected counts candidates the pressure pass refused to consider.
	Protected int
	// Exhausted reports that the pass ran out of evictable records while
	// still over the high watermark.
	Exhausted bool
	// Skipped reports that the pass did not run because the failsafe
	// backoff was still in effect.
	Skipped bool
	// Forced reports that the pass ignored the watermarks because an
	// operator asked for a full eviction.
	Forced bool
}

// EnsureHeadroom runs a collection pass if — and only if — the disk is
// currently over a watermark. It is the cheap guard to call immediately
// before a large pull or before creating a runner environment.
//
// The periodic timer alone loses the race a single job can win: one CI job
// pulling a multi-gigabyte toolchain image can cross the high watermark and
// fill the disk well inside a 60s tick. Checking here costs one syscall on
// the happy path.
//
// Errors are logged, never returned: a GC failure must not fail the job.
func (c *Collector) EnsureHeadroom(ctx context.Context) {
	if c == nil {
		return
	}
	u, err := diskspace.Check(c.cfg.Path)
	if err != nil {
		c.cfg.Log.Debug("headroom check failed", "path", c.cfg.Path, "error", err)
		return
	}
	p := Evaluate(u, c.cfg.Thresholds)
	if !p.Over {
		return
	}
	c.cfg.Log.Info("disk over watermark before pull; collecting images first",
		"path", c.cfg.Path,
		"used_percent", round1(u.UsedPercent()),
		"free_gib", round1(diskspace.GiB(u.FreeBytes)),
		"reason", p.ReasonString())
	if _, err := c.Collect(ctx); err != nil {
		c.cfg.Log.Warn("pre-pull image collection failed", "error", err)
	}
}

// Collect runs one full pass: the optional age backstop, then the
// pressure-driven LRU eviction.
//
// The pressure loop re-reads real disk usage after every deletion rather
// than trusting the planned size estimates, because a layer shared with a
// surviving image frees nothing and an extracted snapshot frees more than
// its packed size. It stops the instant the reading clears both watermarks.
//
// Failsafe: if the pass deletes every record it is allowed to and usage is
// still above the high watermark, the remaining bytes are live data, not
// cache. Collect logs that loudly with the numbers and then suppresses
// further passes for ExhaustedBackoff so the daemon does not spin listing
// and re-planning a store it cannot shrink.
func (c *Collector) Collect(ctx context.Context) (Result, error) {
	return c.collect(ctx, false)
}

// CollectAll runs one pass that evicts EVERY unprotected image record,
// whether or not the disk is over a watermark. It is the operator override
// behind `ephemerd cache clear containerd --all`.
//
// It forces the POLICY, not the safety: images backing running containers
// and the node's pinned runner images are protected here exactly as they are
// in the automatic pass (see PlanForced). It also ignores the exhausted
// backoff — that failsafe exists to stop the daemon spinning on its own
// timer, and a human typing --all is not the daemon's timer.
//
// Cost is a cold image store: the next job on this node re-pulls everything.
// That is the operator's stated intent when they pass --all.
func (c *Collector) CollectAll(ctx context.Context) (Result, error) {
	return c.collect(ctx, true)
}

func (c *Collector) collect(ctx context.Context, force bool) (Result, error) {
	if c == nil {
		return Result{}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	log := c.cfg.Log
	var res Result
	res.Forced = force

	before, err := diskspace.Check(c.cfg.Path)
	if err != nil {
		return res, err
	}
	res.Before, res.After = before, before
	res.Pressure = Evaluate(before, c.cfg.Thresholds)

	if !force && res.Pressure.Over && c.now().Before(c.exhaustedUntil) {
		res.Skipped = true
		log.Debug("image gc suppressed; previous pass exhausted every evictable image",
			"until", c.exhaustedUntil)
		return res, nil
	}
	// Nothing to do: not under pressure and no age backstop configured.
	// A forced pass always has something to do.
	if !force && !res.Pressure.Over && c.cfg.MaxAge <= 0 {
		return res, nil
	}

	nss, err := ResolveNamespaces(ctx, c.cfg.Client, c.cfg.Namespaces, c.cfg.NamespacePrefixes)
	if err != nil {
		return res, err
	}
	if len(nss) == 0 {
		return res, nil
	}

	liveIDs, running, err := RunningContainers(ctx, c.cfg.Client, log)
	if err != nil {
		// Without the live set we cannot guarantee we won't evict an
		// image a running job needs. Abort rather than risk it.
		return res, err
	}
	protected := ProtectedSet(c.cfg.PinnedImages, running)

	cands, err := ListCandidates(ctx, c.cfg.Client, nss, log)
	if err != nil {
		return res, err
	}

	// Expand name-prefix protections for live jobs (BuildKit export
	// records, which no container references).
	if c.cfg.LiveJobPrefixes != nil && len(liveIDs) > 0 {
		var prefixes []string
		for id := range liveIDs {
			prefixes = append(prefixes, c.cfg.LiveJobPrefixes(id)...)
		}
		if n := ProtectPrefixed(cands, prefixes, protected); n > 0 {
			log.Debug("image gc: protected live-job records", "count", n)
		}
	}

	// Optional age backstop first, so the pressure pass plans against
	// what is actually left.
	if c.cfg.MaxAge > 0 {
		cutoff := c.now().UTC().Add(-c.cfg.MaxAge)
		aged := PlanByAge(cands, protected, cutoff)
		if len(aged) > 0 {
			res.AgeEvicted = Evict(ctx, c.cfg.Client, aged, false, log, nil)
			log.Info("image gc: age backstop evicted records",
				"count", res.AgeEvicted, "max_age", c.cfg.MaxAge)
			cands = removeEvicted(cands, aged)
		}
	}

	if !force && !res.Pressure.Over {
		res.After, _ = diskspace.Check(c.cfg.Path)
		return res, nil
	}

	plan := PlanEviction(cands, protected, before, c.cfg.Thresholds)
	// A forced pass takes everything unprotected instead of stopping at the
	// low watermark — that is the whole point of the operator override.
	if force {
		plan = PlanForced(cands, protected)
	}
	res.Protected = plan.Protected

	log.Info("image gc: evicting image records",
		"path", c.cfg.Path,
		"forced", force,
		"used_percent", round1(before.UsedPercent()),
		"free_gib", round1(diskspace.GiB(before.FreeBytes)),
		"total_gib", round1(diskspace.GiB(before.TotalBytes)),
		"reason", plan.ReasonString(),
		"bytes_to_free_gib", round1(diskspace.GiB(plan.BytesToFree)),
		"candidates", len(cands),
		"planned", len(plan.Evict),
		"protected", plan.Protected)

	// Stop as soon as a real reading clears both watermarks; the planned
	// sizes are only a budgeting hint. A forced pass has no stopping point
	// short of the whole plan, so it passes no stop function.
	cleared := false
	var stop func() bool
	if !force {
		stop = func() bool {
			u, err := diskspace.Check(c.cfg.Path)
			if err != nil {
				return false
			}
			res.After = u
			if !Evaluate(u, c.cfg.Thresholds).Over {
				cleared = true
				return true
			}
			return false
		}
	}
	res.Evicted = Evict(ctx, c.cfg.Client, plan.Evict, true, log, stop)

	after, err := diskspace.Check(c.cfg.Path)
	if err == nil {
		res.After = after
		cleared = !Evaluate(after, c.cfg.Thresholds).Over
	}

	// A forced pass was never chasing a watermark, so it must not arm the
	// exhausted-backoff failsafe: doing so would let one operator-run
	// `cache clear --all` silence the AUTOMATIC collector for the next 30
	// minutes on a node that is genuinely filling up.
	if force {
		log.Info("image gc: forced pass complete",
			"evicted", res.Evicted,
			"protected", res.Protected,
			"used_percent", round1(res.After.UsedPercent()),
			"free_gib", round1(diskspace.GiB(res.After.FreeBytes)),
			"reclaimed_gib", round1(diskspace.GiB(saturatingSub(res.After.FreeBytes, before.FreeBytes))))
		return res, nil
	}

	switch {
	case cleared:
		log.Info("image gc: complete",
			"evicted", res.Evicted,
			"used_percent", round1(res.After.UsedPercent()),
			"free_gib", round1(diskspace.GiB(res.After.FreeBytes)),
			"reclaimed_gib", round1(diskspace.GiB(saturatingSub(res.After.FreeBytes, before.FreeBytes))))
	default:
		// Everything evictable is gone and we are still over the line:
		// what remains is live data (running containers, pinned runner
		// images, the runner tree itself), not reclaimable cache.
		res.Exhausted = true
		c.exhaustedUntil = c.now().Add(c.cfg.ExhaustedBackoff)
		log.Error("image gc: still over the high watermark with nothing left to evict — remaining usage is live data, not cache; free space manually or add capacity",
			"path", c.cfg.Path,
			"evicted", res.Evicted,
			"protected", res.Protected,
			"used_percent", round1(res.After.UsedPercent()),
			"high_watermark_percent", c.cfg.Thresholds.HighUsedPercent,
			"free_gib", round1(diskspace.GiB(res.After.FreeBytes)),
			"min_free_gib", round1(diskspace.GiB(c.cfg.Thresholds.MinFreeBytes)),
			"total_gib", round1(diskspace.GiB(res.After.TotalBytes)),
			"suppressed_for", c.cfg.ExhaustedBackoff)
	}

	return res, nil
}

// removeEvicted returns cands minus everything in gone, matched on Key.
func removeEvicted(cands, gone []Candidate) []Candidate {
	if len(gone) == 0 {
		return cands
	}
	drop := make(map[string]struct{}, len(gone))
	for _, g := range gone {
		drop[g.Key()] = struct{}{}
	}
	out := cands[:0]
	for _, c := range cands {
		if _, ok := drop[c.Key()]; ok {
			continue
		}
		out = append(out, c)
	}
	return out
}

// saturatingSub is a-b, floored at zero (free space can go down during a
// pass if a job is writing while we collect).
func saturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// round1 trims a float to one decimal so log lines stay readable.
func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}
