package buildkit

import (
	"time"

	bkclient "github.com/moby/buildkit/client"
)

// GCConfig bounds the on-disk BuildKit build cache.
//
// WHY THIS EXISTS: BuildKit only garbage-collects when its worker is
// constructed with a non-empty GC policy — control.Controller's gc() is
// literally `if policy := w.GCPolicy(); len(policy) > 0 { w.Prune(...) }`.
// ephemerd built its worker without one, so the shared "buildkit"
// containerd namespace grew without bound: every `docker build` in every CI
// job added cache records, snapshots and `containerd.io/gc.flat` leases that
// nothing ever released. Measured on a production node: 76 images, 302
// snapshots, 481 leases and ~44 GB of a 116 GB disk, spanning 49 long-dead
// jobs going back two and a half weeks. That, not runner image retention,
// is what filled the disk until QEMU froze the VM.
//
// The goal is a WARM BUT BOUNDED cache. Pruning aggressively would defeat
// the point of a build cache and push the cost onto the network, which we
// are separately trying to reduce; leaving it unbounded is what caused the
// outage. So: a floor that is never collected, a ceiling that always is,
// and a free-space guard that overrides both.
type GCConfig struct {
	// Disabled turns BuildKit's garbage collection off entirely, restoring
	// the previous (unbounded) behavior. Only useful for debugging a
	// cache-correctness problem.
	Disabled bool

	// ReservedBytes is cache that is never collected, even when idle. This
	// is the warm floor: below it BuildKit keeps everything, so the common
	// "same repo builds again an hour later" case still hits cache.
	ReservedBytes int64

	// MaxUsedBytes is the hard ceiling on total build cache. Anything above
	// it is collected regardless of age.
	MaxUsedBytes int64

	// MinFreeBytes makes GC collect whatever it must to keep at least this
	// much free space on the filesystem, overriding ReservedBytes. This is
	// the arm that saves the node when something else (runner images, job
	// workdirs) is consuming the disk.
	MinFreeBytes int64

	// KeepDuration is the age after which cache records are collected once
	// usage is above ReservedBytes.
	KeepDuration time.Duration

	// EphemeralKeepDuration and EphemeralMaxUsedBytes bound the cheaply
	// reproducible record types — local build contexts, RUN --mount=cache
	// mounts and git checkouts. Re-creating those costs a copy, not a
	// network round trip, so they are collected far more eagerly than
	// layer cache. Mirrors the first rule of BuildKit's own default policy.
	EphemeralKeepDuration time.Duration
	EphemeralMaxUsedBytes int64
}

// ephemeralFilters selects the record types that are cheap to reproduce.
// Same list BuildKit's DefaultGCPolicy uses for its first rule.
var ephemeralFilters = []string{"type==source.local,type==exec.cachemount,type==source.git.checkout"}

// PruneInfo renders the config as the BuildKit prune rules a worker's GC
// policy is made of. Pure — no disk access, no clock — so the resulting
// rule set is unit-testable without standing up a worker.
//
// Returns nil when collection is disabled or nothing is configured, which
// is exactly the "never collect" state that caused the leak; callers that
// want a bounded cache must supply a non-zero bound.
//
// Rules are evaluated in order and are complementary: the first keeps
// ephemeral records from ever becoming a large share of the cache, the
// second ages records out, and the third is the unconditional size bound.
//
// THE THIRD RULE IS LOAD-BEARING AND MUST NOT CARRY A KeepDuration.
// BuildKit applies KeepDuration as an absolute exemption within a rule —
// cache/manager.go's pruneOnce skips every record whose lastUsedAt is
// newer than now-KeepDuration BEFORE it looks at MaxUsedSpace. So a rule
// that sets both an age and a size bound does not mean "collect anything
// over the cap, and additionally anything older than the age"; it means
// "collect only records older than the age, and only once over the cap".
// With the default 168h that leaves a week's worth of build cache
// completely exempt from the ceiling, which on a node that rebuilds the
// same images several times a day is indistinguishable from having no
// ceiling at all. Measured in a Linux VM against a real containerd +
// BuildKit: 18 builds of a unique 200 MB layer with reserved=1 GiB,
// max_used=2 GiB and keep_duration=168h grew the cache to 4.4 GB and
// climbing; the identical workload with the size bound applied
// unconditionally settled at 2.1 GB. BuildKit's own DefaultGCPolicy has
// the same shape — its last two rules carry no KeepDuration for exactly
// this reason.
func (g GCConfig) PruneInfo() []bkclient.PruneInfo {
	if g.Disabled {
		return nil
	}

	var out []bkclient.PruneInfo

	if g.EphemeralMaxUsedBytes > 0 || g.EphemeralKeepDuration > 0 {
		out = append(out, bkclient.PruneInfo{
			Filter:       ephemeralFilters,
			KeepDuration: g.EphemeralKeepDuration,
			MaxUsedSpace: g.EphemeralMaxUsedBytes,
		})
	}

	sized := g.ReservedBytes > 0 || g.MaxUsedBytes > 0 || g.MinFreeBytes > 0

	if sized || g.KeepDuration > 0 {
		out = append(out, bkclient.PruneInfo{
			KeepDuration:  g.KeepDuration,
			ReservedSpace: g.ReservedBytes,
			MaxUsedSpace:  g.MaxUsedBytes,
			MinFreeSpace:  g.MinFreeBytes,
		})
	}

	if sized {
		out = append(out, bkclient.PruneInfo{
			ReservedSpace: g.ReservedBytes,
			MaxUsedSpace:  g.MaxUsedBytes,
			MinFreeSpace:  g.MinFreeBytes,
		})
	}

	return out
}

// Enabled reports whether this config produces any prune rule at all — i.e.
// whether the build cache is bounded.
func (g GCConfig) Enabled() bool { return len(g.PruneInfo()) > 0 }
