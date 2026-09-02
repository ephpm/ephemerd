package imagegc

import (
	"sort"
	"strings"
	"time"

	"github.com/ephpm/ephemerd/pkg/diskspace"
)

// Thresholds describes when image garbage collection starts and when it
// stops. Two independent arms trigger a pass, and whichever demands more
// reclaimed bytes wins:
//
//   - a percentage arm (HighUsedPercent triggers, LowUsedPercent is the
//     target), and
//   - an absolute arm (MinFreeBytes triggers, TargetFreeBytes is the target).
//
// Both arms exist because neither is safe alone. 15% free of a 1 TB node is
// 150 GB — evicting there is pointless churn. 15% free of a 100 GB node is
// 15 GB, which three concurrent jobs writing ~5 GB of container layers each
// can consume before the next tick, so a percentage alone under-protects a
// small disk. Conversely a fixed 20 GB floor under-protects a large busy
// node. Setting both and taking the more conservative answer covers both
// shapes.
//
// Using two watermarks rather than one line is what stops the collector
// thrashing: a single threshold would evict one image, drop just under, and
// evict again on the very next tick forever. Freeing down to a distinctly
// lower low-water mark buys many ticks of headroom per pass. This is
// kubelet's image-GC model.
//
// A non-positive value disables that arm. Zeroing everything disables
// pressure-triggered collection entirely.
type Thresholds struct {
	// HighUsedPercent triggers a pass when disk used% reaches it.
	HighUsedPercent float64
	// LowUsedPercent is the used% a triggered pass tries to get back to.
	// Must be below HighUsedPercent to be useful; a value at or above it
	// is clamped to HighUsedPercent (degrading to single-threshold
	// behavior rather than erroring at eviction time).
	LowUsedPercent float64
	// MinFreeBytes triggers a pass when free space falls below it.
	MinFreeBytes uint64
	// TargetFreeBytes is the free-space level a triggered pass tries to
	// reach. Values below MinFreeBytes are clamped up to it.
	TargetFreeBytes uint64
}

// Enabled reports whether any arm is configured.
func (t Thresholds) Enabled() bool {
	return t.HighUsedPercent > 0 || t.MinFreeBytes > 0
}

// Trigger reasons reported by Pressure.Reasons.
const (
	ReasonUsedPercent = "used_percent"
	ReasonMinFree     = "min_free"
)

// Pressure is the verdict for one capacity reading.
type Pressure struct {
	// Over reports whether any arm triggered.
	Over bool
	// Reasons lists the arms that triggered, in a stable order
	// (ReasonUsedPercent before ReasonMinFree).
	Reasons []string
	// BytesToFree is how much must be reclaimed to satisfy every
	// triggered arm's target. Zero when Over is false.
	BytesToFree uint64
}

// ReasonString joins Reasons for logging.
func (p Pressure) ReasonString() string { return strings.Join(p.Reasons, "+") }

// Evaluate reports whether a capacity reading is over either trigger and how
// many bytes a pass must reclaim. Pure — no syscalls, no clock.
//
// A reading with TotalBytes == 0 (an unreadable or stubbed probe) never
// triggers: guessing under uncertainty would delete a warm cache for no
// reason.
func Evaluate(u diskspace.Usage, t Thresholds) Pressure {
	if u.TotalBytes == 0 {
		return Pressure{}
	}

	var p Pressure

	if t.HighUsedPercent > 0 && u.UsedPercent() >= t.HighUsedPercent {
		low := t.LowUsedPercent
		if low <= 0 || low > t.HighUsedPercent {
			low = t.HighUsedPercent
		}
		allowedUsed := uint64(float64(u.TotalBytes) * low / 100)
		if used := u.UsedBytes(); used > allowedUsed {
			if need := used - allowedUsed; need > p.BytesToFree {
				p.BytesToFree = need
			}
		}
		p.Over = true
		p.Reasons = append(p.Reasons, ReasonUsedPercent)
	}

	if t.MinFreeBytes > 0 && u.FreeBytes < t.MinFreeBytes {
		target := t.TargetFreeBytes
		if target < t.MinFreeBytes {
			target = t.MinFreeBytes
		}
		if target > u.FreeBytes {
			if need := target - u.FreeBytes; need > p.BytesToFree {
				p.BytesToFree = need
			}
		}
		p.Over = true
		p.Reasons = append(p.Reasons, ReasonMinFree)
	}

	// Triggered exactly at the target (low == high, or free == target).
	// Ask for a token byte so the pass evicts the single least-recently
	// used image instead of returning an empty plan and re-triggering on
	// the next tick with nothing to show for it.
	if p.Over && p.BytesToFree == 0 {
		p.BytesToFree = 1
	}

	return p
}

// Candidate is one image record considered for eviction.
//
// SizeBytes is the image's packed content size and is only ever an
// estimate of what eviction reclaims: layers shared with another image are
// counted here but will not actually be freed, and the extracted snapshot
// (typically larger than the packed layers) is not counted at all. The plan
// therefore treats it as a budgeting hint; Collector re-reads real disk
// usage after every deletion and stops the moment pressure clears. A
// non-positive SizeBytes means "unknown" and contributes nothing to the
// budget, which makes such candidates always eligible rather than
// invisible.
type Candidate struct {
	Namespace    string
	Name         string
	LastAccessed time.Time
	SizeBytes    int64
}

// Key is a stable namespace-qualified identifier, used for deterministic
// tie-breaking and in log lines.
func (c Candidate) Key() string { return c.Namespace + "/" + c.Name }

// Plan is the outcome of a pure eviction decision.
type Plan struct {
	Pressure
	// Evict lists the records to delete, least-recently-used first.
	Evict []Candidate
	// Protected counts candidates skipped because they are in the
	// protected set.
	Protected int
	// PlannedBytes is the summed SizeBytes of Evict (unknown sizes count
	// as zero).
	PlannedBytes uint64
	// Shortfall is BytesToFree minus PlannedBytes when every evictable
	// candidate was selected and the estimate still falls short. Non-zero
	// means "there may be nothing left to reclaim" and drives the
	// failsafe warning — see Collector.Collect.
	Shortfall uint64
}

// PlanEviction decides which image records to evict, in what order.
//
// This is the whole eviction policy as one pure function: disk pressure is
// the TRIGGER (via Evaluate), least-recently-used is the ORDER, the
// protected set is an absolute veto, and the low watermark is the stopping
// point. It touches no containerd, no filesystem and no clock, so the
// policy is unit-testable on its own.
//
// protected is keyed by image name (ref), not by Key: an image that a
// running container references, or that is the node's pinned runner image,
// must survive in every namespace it appears in.
//
// Candidates sort by LastAccessed ascending, ties broken by Key so repeated
// runs over the same store produce the same plan.
func PlanEviction(candidates []Candidate, protected map[string]struct{}, usage diskspace.Usage, t Thresholds) Plan {
	p := Plan{Pressure: Evaluate(usage, t)}
	if !p.Over {
		return p
	}

	eligible := filterProtected(candidates, protected, &p.Protected)
	sortLRU(eligible)

	for _, c := range eligible {
		p.Evict = append(p.Evict, c)
		if c.SizeBytes > 0 {
			p.PlannedBytes += uint64(c.SizeBytes)
		}
		if p.PlannedBytes >= p.BytesToFree {
			return p
		}
	}

	p.Shortfall = p.BytesToFree - p.PlannedBytes
	return p
}

// PlanForced selects EVERY unprotected candidate, least-recently-used
// first, regardless of disk pressure. It is the operator override behind
// `ephemerd cache clear containerd --all`. Pure.
//
// WHY THIS IS SEPARATE FROM PlanEviction. PlanEviction is watermark-driven
// by design: on a node whose thresholds are not tripped it correctly returns
// an empty plan, which is right for the timer and wrong for a human who has
// just decided this node's image store must go. Before this existed the
// `--all` flag was accepted, silently dropped for the containerd target, and
// reported "0 records removed" — so the operator concluded the command was
// broken rather than that the policy had declined (observed while chasing
// disk on a Windows node).
//
// The protected set is still an ABSOLUTE veto here, exactly as in
// PlanEviction: --all forces the POLICY open, never the safety. An image
// backing a running container or listed as a pinned runner image is never
// selected, because evicting one breaks a job in flight or costs every
// subsequent job a re-pull — neither is something an operator asking to
// reclaim cache is asking for.
func PlanForced(candidates []Candidate, protected map[string]struct{}) Plan {
	var p Plan
	p.Over = true
	p.Reasons = []string{ReasonForced}

	eligible := filterProtected(candidates, protected, &p.Protected)
	sortLRU(eligible)
	p.Evict = eligible
	for _, c := range eligible {
		if c.SizeBytes > 0 {
			p.PlannedBytes += uint64(c.SizeBytes)
		}
	}
	return p
}

// ReasonForced is the Pressure reason PlanForced reports: the pass ran
// because an operator asked for it, not because a watermark tripped.
const ReasonForced = "forced"

// PlanByAge selects every candidate last accessed before cutoff,
// least-recently-used first, skipping the protected set. It is the optional
// age backstop (`cache_max_age`), not the primary mechanism: age alone
// discards a warm cache while the disk is half empty and forces re-downloads
// we are otherwise trying to eliminate. Pure.
//
// A candidate with a zero LastAccessed is never selected — a record with no
// usable timestamp is treated as unknown, not as ancient.
func PlanByAge(candidates []Candidate, protected map[string]struct{}, cutoff time.Time) []Candidate {
	var skipped int
	eligible := filterProtected(candidates, protected, &skipped)
	sortLRU(eligible)

	var out []Candidate
	for _, c := range eligible {
		if c.LastAccessed.IsZero() || !c.LastAccessed.Before(cutoff) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// ProtectPrefixed adds to protected every candidate whose Name starts with
// one of prefixes, and returns how many were added.
//
// Some records are tied to a live job by NAME rather than by container
// reference: BuildKit writes each job's build output into the one shared
// "buildkit" namespace under a name scoped by that job's container ID, and
// no container ever references those records. Evicting one mid-job breaks
// the job's subsequent `docker push`, so the live jobs' prefixes are
// expanded into the protected set before planning. Pure.
func ProtectPrefixed(candidates []Candidate, prefixes []string, protected map[string]struct{}) int {
	if len(prefixes) == 0 || protected == nil {
		return 0
	}
	added := 0
	for _, c := range candidates {
		if _, already := protected[c.Name]; already {
			continue
		}
		for _, p := range prefixes {
			if p == "" || !strings.HasPrefix(c.Name, p) {
				continue
			}
			protected[c.Name] = struct{}{}
			added++
			break
		}
	}
	return added
}

// filterProtected returns the candidates whose Name is not in protected,
// incrementing *skipped for each veto.
func filterProtected(candidates []Candidate, protected map[string]struct{}, skipped *int) []Candidate {
	out := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := protected[c.Name]; ok {
			*skipped++
			continue
		}
		out = append(out, c)
	}
	return out
}

// sortLRU orders candidates least-recently-used first, with Key as a
// deterministic tie-break.
func sortLRU(c []Candidate) {
	sort.Slice(c, func(i, j int) bool {
		if !c[i].LastAccessed.Equal(c[j].LastAccessed) {
			return c[i].LastAccessed.Before(c[j].LastAccessed)
		}
		return c[i].Key() < c[j].Key()
	})
}
