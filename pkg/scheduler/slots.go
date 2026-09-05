package scheduler

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Concurrency slots.
//
// Every dispatch path holds one slot for the whole life of the job it
// provisions: s.sem (local runners), s.linuxSem (Linux dispatch to the VM
// worker), s.macSem (macOS VMs). The buffered channel IS the accounting —
// len() is how many slots are held, cap() is the pool size — so nothing here
// keeps a parallel counter that could drift from the semaphore it describes.
//
// Three rules, all of them written after an outage:
//
//  1. A slot is released the moment the job it represents stops being demand,
//     i.e. as soon as the job is untracked. NEVER "once teardown finishes":
//     teardown is precisely the thing that hangs. (PR #190, local + Linux
//     paths; issue #196 extended it to the macOS wait-goroutine, which held
//     the slot across macVM.Stop() and a ReleaseJob call on
//     context.Background().)
//
//  2. Release is idempotent and has a deferred backstop, so an error return
//     added later cannot forget it and a panic cannot skip it.
//
//  3. Nothing that can block forever may sit between the acquire and the
//     release on a failure path. This is the whole of issue #196: the macOS
//     provisioning watchdog force-stopped the wedged VM "to reclaim the slot"
//     and then blocked FOREVER waiting for the wedged wait goroutine to
//     unwind, so the release it was on its way to never ran. The node held
//     the sole macOS slot for 28 hours while reporting status: ok and
//     active_jobs: 0 (the job had never been tracked), and every macOS job
//     queued in that window aged out at GitHub's 24h timeout without running.

// These are vars rather than consts only so tests can compress the timeline;
// nothing outside tests reassigns them.
var (
	// slotWaitLogAfter is how long a dispatch must sit blocked on a full pool
	// before it says so. Short enough that a genuinely stuck pool is visible
	// within a minute of the next job arriving, long enough that ordinary
	// queueing behind a running job on a max_concurrent = 1 node does not
	// narrate itself.
	slotWaitLogAfter = 30 * time.Second

	// slotWaitLogEvery re-states a still-blocked wait on this interval. A job
	// can legitimately wait hours behind a long build, so this is quiet; the
	// point is that a pool which is stuck forever keeps saying so instead of
	// going silent after one line. Issue #196's log had NOTHING in it for 28
	// hours — five macOS jobs blocked on the leaked slot and not one of them
	// left a trace.
	slotWaitLogEvery = 5 * time.Minute

	// slotLeakSuspectAfter is when a blocked wait escalates from "busy" to
	// "broken". A slot that is held while the scheduler tracks NO running job
	// is either a provision in flight (bounded: minutes) or a leak (forever),
	// so waiting this long against a pool that nothing is tracked against is
	// reported at Error.
	slotLeakSuspectAfter = 15 * time.Minute
)

// slotToken is a one-shot handle to an acquired concurrency slot.
//
// It replaces the bare `<-s.macSem` that each of handleMacOSJob's six error
// returns had to remember on its own. The type exists so that "did this path
// release?" is answered by construction (a deferred backstop) rather than by
// reading every return statement.
type slotToken struct {
	sem  chan struct{}
	once sync.Once
}

// release returns the slot to its pool. Safe to call any number of times from
// any goroutine: only the first call drains the semaphore, so an explicit
// early release (the one that matters — it is what lets the next job in) can
// coexist with a deferred backstop without under-draining the channel and
// handing out a slot somebody else still holds.
func (t *slotToken) release() {
	if t == nil {
		return
	}
	t.once.Do(func() { <-t.sem })
}

// acquireSlot takes a slot from sem, blocking until one is free or ctx is
// done. It returns nil when ctx ended first; the caller must not provision.
//
// The only behavioural difference from the raw `select { case sem <- ... }`
// it replaces is that a wait which actually BLOCKS becomes visible in the log.
// A saturated pool and a leaked pool look identical from outside the process,
// and until issue #196 both looked identical from inside it too: the daemon
// logged nothing at all while five macOS jobs piled up behind a slot that
// would never come back.
func (s *Scheduler) acquireSlot(ctx context.Context, sem chan struct{}, pool string, log *slog.Logger) *slotToken {
	// Fast path: a free slot logs nothing and arms no timer, which is the
	// overwhelmingly common case. (It still allocates the token — one tiny
	// struct per job, against a job that is about to boot a VM.)
	select {
	case sem <- struct{}{}:
		return &slotToken{sem: sem}
	case <-ctx.Done():
		return nil
	default:
	}

	start := time.Now()
	timer := time.NewTimer(slotWaitLogAfter)
	defer timer.Stop()
	for {
		select {
		case sem <- struct{}{}:
			log.Info("acquired concurrency slot after waiting",
				"pool", pool, "waited", time.Since(start).Truncate(time.Second),
				"held", len(sem), "capacity", cap(sem))
			return &slotToken{sem: sem}
		case <-ctx.Done():
			return nil
		case <-timer.C:
			// PER-POOL, not global. The first version of this compared the
			// pool's held count against s.ActiveJobs(), which is
			// len(s.running) across EVERY pool — so a single tracked Linux
			// job running on the mac was enough to make a genuinely leaked
			// macOS slot look accounted for and keep the escalation at Warn.
			// On the #196 node that is not a hypothetical: the mac serves
			// Linux jobs in its embedded VM continuously, so the one signal
			// that would have named the outage was suppressed by unrelated
			// work.
			held, tracked, waited := len(sem), s.trackedInPool(pool), time.Since(start)
			level, msg := slotWaitSeverity(waited, held, tracked)
			log.Log(ctx, level, msg,
				"pool", pool,
				"held", held,
				"capacity", cap(sem),
				"pool_tracked_jobs", tracked,
				"waited", waited.Truncate(time.Second))
			timer.Reset(slotWaitLogEvery)
		}
	}
}

// slotWaitSeverity decides how loudly a blocked dispatch complains.
//
// This is the leak watchdog, and it lives here rather than in a periodic
// sweeper on purpose: a held slot is only a problem when something is waiting
// for it, and a waiting dispatch is exactly this code path. It therefore
// costs nothing when the node is idle and cannot produce a false alarm on a
// node that simply has no macOS work.
//
// held > 0 with poolTrackedJobs == 0 means the slot is charged to a job the
// scheduler is not tracking. That is legitimate for the length of a
// provision (bounded by MacOSProvisionTimeout, minutes) and illegitimate
// forever after — it is the exact signature of the 28-hour macOS outage,
// where `ephemerd status` reported active_jobs: 0 on a node whose only slot
// had been held since the previous afternoon. Pure so the escalation rule is
// testable without a real pool or a real wait.
//
// poolTrackedJobs is scoped to THE POOL BEING WAITED ON (see trackedInPool),
// never to the whole scheduler. held is a per-pool number, so comparing it to
// a global job count mixes units: on a mac, which serves Linux jobs from its
// embedded VM alongside macOS VMs, one tracked Linux job made a leaked macOS
// slot look explained and downgraded this to Warn forever.
func slotWaitSeverity(waited time.Duration, held, poolTrackedJobs int) (slog.Level, string) {
	if held > 0 && poolTrackedJobs == 0 && waited >= slotLeakSuspectAfter {
		return slog.LevelError, "blocked on a concurrency slot that no tracked job accounts for — suspected slot leak"
	}
	return slog.LevelWarn, "waiting for a free concurrency slot"
}

// poolOf reports which dispatch pool a tracked job's slot came from. It
// mirrors the routing in handleQueued: macOS jobs hold macSem, Linux jobs
// dispatched into the VM worker hold linuxSem, everything else holds sem.
func poolOf(rj *runningJob) string {
	switch {
	case rj.macosVM != nil:
		return "macos"
	case rj.dispatched != "":
		return "linux"
	default:
		return "local"
	}
}

// trackedInPool counts tracked running jobs whose slot came from pool.
//
// This is the denominator the leak watchdog needs: "is anything the scheduler
// knows about accounting for the slots THIS pool is holding". ActiveJobs() —
// the global count — cannot answer that on a node that runs more than one
// kind of job, and a mac runs three.
func (s *Scheduler) trackedInPool(pool string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, rj := range s.running {
		if poolOf(rj) == pool {
			n++
		}
	}
	return n
}

// SlotUsage reports concurrency-slot occupancy for every dispatch pool.
//
// Held is read from the semaphore itself, so it counts slots that are charged
// to an UNTRACKED job — a provision still in flight, or a leak. That is the
// whole point: ActiveJobs() counts len(s.running) and so reported 0 on the
// node that had been wedged for 28 hours. A pool at held == capacity with
// nothing tracked against it is the signal that was missing.
func (s *Scheduler) SlotUsage() []SlotStats {
	return []SlotStats{
		{Pool: "local", Held: len(s.sem), Capacity: cap(s.sem)},
		{Pool: "linux", Held: len(s.linuxSem), Capacity: cap(s.linuxSem)},
		{Pool: "macos", Held: len(s.macSem), Capacity: cap(s.macSem)},
	}
}

// SlotStats is one dispatch pool's occupancy.
type SlotStats struct {
	Pool     string `json:"pool"`
	Held     int    `json:"held"`
	Capacity int    `json:"capacity"`
}

// HeldSlots is the total number of concurrency slots currently held across
// all pools, tracked or not.
func (s *Scheduler) HeldSlots() int {
	total := 0
	for _, p := range s.SlotUsage() {
		total += p.Held
	}
	return total
}

// SlotCapacity is the total number of concurrency slots across all pools.
func (s *Scheduler) SlotCapacity() int {
	total := 0
	for _, p := range s.SlotUsage() {
		total += p.Capacity
	}
	return total
}

// ---------------------------------------------------------------------------
// Abandoned macOS VMs.
//
// The #196 fix trades a leaked concurrency slot for a leaked goroutine: when a
// Vz stop does not return, we walk away from it and give the slot back. That
// trade is right, but it is not free, and until this registry existed it was
// also UNBOUNDED and INVISIBLE.
//
// What one abandonment actually costs. Three goroutines (the WaitForRunner
// wait, abandonMacProvision's stopVMAsync, and — before the fix below —
// the caller's redundant stopVMBounded blocking inside darwinMacOSVM's
// stopOnce), each retaining a live *darwinMacOSVM, and, far more expensively,
// a STILL-RUNNING macOS guest holding its whole configured RAM
// (MacOSVMConfig.MemoryMB — gigabytes) plus its disk clone.
//
// What used to bound it. The semaphore, by accident: leak the only slot and
// the node provisions zero further macOS VMs, so exactly one guest could ever
// be stranded. Releasing the slot on the failure path is precisely what
// removed that accidental cap, and MaxMacOSVMs no longer charges for abandoned
// VMs because they are not tracked and no longer hold a slot. Left alone, a
// mac with a repeatably wedging guest would keep provisioning until
// Virtualization.framework refused (its concurrent macOS-guest limit is low —
// about two on Apple Silicon) or the host ran out of memory.
//
// So the bound is explicit now: count them, cap them, say so loudly, and
// report the count next to held_slots. A node that has given up on more than a
// couple of guests is broken in a way a restart fixes and no further
// provisioning will; refusing new macOS work there turns unbounded RAM growth
// into a legible, bounded failure that `ephemerd status` names.
// ---------------------------------------------------------------------------

// defaultMacAbandonedVMCap is how many macOS VMs may be abandoned-but-not-
// confirmed-dead before the macOS pool stops taking work.
//
// Deliberately tiny. Each entry is a live guest holding multiple gigabytes,
// and Virtualization.framework itself only permits about two concurrent macOS
// guests on Apple Silicon — so the useful budget is "one transient hiccup",
// not "keep trying".
const defaultMacAbandonedVMCap = 2

// macVMRef identifies one macOS VM instance for the abandoned registry and for
// the log lines about it. VMID is the PER-ATTEMPT instance id (see
// macVMInstanceID), not the job id: two provisioning attempts for the same job
// are two different VMs and must be countable separately.
type macVMRef struct {
	JobID int64
	VMID  string
}

// AbandonedMacVM is one macOS VM whose teardown was given up on, as reported
// by status/healthz.
type AbandonedMacVM struct {
	JobID   int64  `json:"job_id"`
	VMID    string `json:"vm_id"`
	Reason  string `json:"reason"`
	HeldFor string `json:"held_for"`
}

type abandonedEntry struct {
	ref    macVMRef
	reason string
	since  time.Time
}

// abandonedMacVMs is the registry. Entries are added when a stop is walked
// away from and removed if that stop ever returns — which does happen: a Vz
// stop can be slow rather than wedged, and a node that recovers must be
// allowed to serve macOS jobs again without a restart.
type abandonedMacVMs struct {
	mu   sync.Mutex
	next uint64
	live map[uint64]abandonedEntry
}

func (a *abandonedMacVMs) charge(ref macVMRef, reason string) (uint64, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.live == nil {
		a.live = make(map[uint64]abandonedEntry)
	}
	a.next++
	id := a.next
	a.live[id] = abandonedEntry{ref: ref, reason: reason, since: time.Now()}
	return id, len(a.live)
}

// discharge removes an entry and reports the remaining count and how long the
// VM was outstanding.
func (a *abandonedMacVMs) discharge(id uint64) (time.Duration, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.live[id]
	delete(a.live, id)
	remaining := len(a.live)
	if !ok {
		return 0, remaining
	}
	return time.Since(e.since), remaining
}

func (a *abandonedMacVMs) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.live)
}

func (a *abandonedMacVMs) snapshot() []AbandonedMacVM {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AbandonedMacVM, 0, len(a.live))
	for _, e := range a.live {
		out = append(out, AbandonedMacVM{
			JobID:   e.ref.JobID,
			VMID:    e.ref.VMID,
			Reason:  e.reason,
			HeldFor: time.Since(e.since).Truncate(time.Second).String(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out
}

// AbandonedMacOSVMs is how many macOS VMs this daemon has given up on and not
// yet seen die. Non-zero means guests are holding host RAM that nothing in
// this process can reclaim; at or above the cap the macOS pool is cordoned.
func (s *Scheduler) AbandonedMacOSVMs() int { return s.macAbandoned.count() }

// AbandonedMacOSVMDetails lists them, newest first by instance id.
func (s *Scheduler) AbandonedMacOSVMDetails() []AbandonedMacVM { return s.macAbandoned.snapshot() }

// abandonedMacVMCap is the effective cap; the field exists so tests can set it
// to 1 instead of staging two wedged guests.
func (s *Scheduler) abandonedMacVMCap() int {
	if s.macAbandonedCap > 0 {
		return s.macAbandonedCap
	}
	return defaultMacAbandonedVMCap
}

// macPoolCordoned reports whether the macOS pool has given up on so many VMs
// that it must stop provisioning, and how many.
//
// Checked twice on the dispatch path for the same reason the #154 cordon is:
// once before blocking on the semaphore, and once after — a dispatch parked on
// a full pool can sit there for the entire life of the job ahead of it, and
// the node can break underneath it in that window.
func (s *Scheduler) macPoolCordoned() (int, int, bool) {
	n, limit := s.macAbandoned.count(), s.abandonedMacVMCap()
	return n, limit, n >= limit
}

// abandonVM charges a walked-away-from teardown to the registry and arranges
// for it to be discharged if the stop ever returns.
//
// done is the channel stopVMAsync returns. If it is already closed the stop
// landed in the race between the caller's deadline and this call, and nothing
// is abandoned — that distinction matters, because charging a VM that is in
// fact dead would cordon a healthy node.
func (s *Scheduler) abandonVM(done <-chan struct{}, ref macVMRef, reason string, log *slog.Logger) {
	select {
	case <-done:
		return // teardown finished after all; nothing was abandoned
	default:
	}

	id, n := s.macAbandoned.charge(ref, reason)
	limit := s.abandonedMacVMCap()
	log.Error("abandoned a macOS VM whose teardown never returned; its guest is still holding host RAM",
		"vm", ref.VMID,
		"job_id", ref.JobID,
		"reason", reason,
		"abandoned_macos_vms", n,
		"cap", limit,
		"detail", "walking away is what keeps the concurrency slot from leaking (#196), but the guest is not gone — only a daemon restart reclaims it")
	if n >= limit {
		log.Error("macOS pool cordoned: too many macOS VMs abandoned without confirmed death — refusing further macOS provisioning on this node",
			"abandoned_macos_vms", n,
			"cap", limit,
			"detail", "each abandoned guest holds its full configured RAM and Virtualization.framework allows only a couple of concurrent macOS guests; provisioning past this point buys nothing and can OOM the host. Restart ephemerd to clear them.")
	}

	// Discharge if it ever finishes. A Vz stop can be merely slow, and a node
	// that recovers on its own must not need a restart to serve macOS again.
	go func() {
		<-done
		held, remaining := s.macAbandoned.discharge(id)
		log.Warn("a previously abandoned macOS VM finally finished stopping",
			"vm", ref.VMID,
			"job_id", ref.JobID,
			"held_for", held.Truncate(time.Second),
			"abandoned_macos_vms", remaining)
	}()
}

// provisionUnwindGrace and teardownGrace are the effective bounds on macOS VM
// teardown. They read from the scheduler so tests can compress them, and fall
// back to the package defaults for a zero-valued Scheduler.
func (s *Scheduler) provisionUnwindGrace() time.Duration {
	if s.macUnwindGrace > 0 {
		return s.macUnwindGrace
	}
	return macProvisionUnwindGrace
}

func (s *Scheduler) teardownGrace() time.Duration {
	if s.macStopGrace > 0 {
		return s.macStopGrace
	}
	return macTeardownGrace
}

// awaitUnwind waits for an abandoned provisioning attempt's teardown
// (stopped) and its wait goroutine (unwound) to finish, but only for grace.
// It reports whether both arrived in time.
//
// This bound is the issue #196 fix in one function. The previous code did the
// moral equivalent of `<-resCh` with no deadline, on the reasoning that
// force-stopping the VM drops the guest connection and therefore unblocks the
// reachability wait. On metal that reasoning failed: the VM's own stop path
// timed out ("macOS VM did not stop gracefully, forcing") and the guest wait
// never returned, so the dispatch goroutine parked there permanently — still
// holding the macOS slot it had just logged that it was reclaiming.
//
// A wedged guest wait is already a leaked goroutine and nothing in this
// process can kill it. Blocking on it converts that leaked goroutine into a
// leaked concurrency slot, which is the difference between a wasted job and a
// dead node. So: give teardown a bounded chance to be tidy, then walk away.
func awaitUnwind(stopped, unwound <-chan struct{}, grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for stopped != nil || unwound != nil {
		// Prefer an already-finished signal over the deadline: with both
		// ready, a plain select picks at random and would report a clean
		// unwind as a timeout.
		select {
		case <-stopped:
			stopped = nil
			continue
		case <-unwound:
			unwound = nil
			continue
		default:
		}
		select {
		case <-stopped:
			stopped = nil
		case <-unwound:
			unwound = nil
		case <-timer.C:
			return false
		}
	}
	return true
}
