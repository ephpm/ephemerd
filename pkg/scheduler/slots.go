package scheduler

import (
	"context"
	"log/slog"
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
	// Fast path: a free slot logs nothing, which is the overwhelmingly
	// common case and must stay allocation- and timer-free.
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
			held, tracked, waited := len(sem), s.ActiveJobs(), time.Since(start)
			level, msg := slotWaitSeverity(waited, held, tracked)
			log.Log(ctx, level, msg,
				"pool", pool,
				"held", held,
				"capacity", cap(sem),
				"tracked_jobs", tracked,
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
// held > 0 with trackedJobs == 0 means the slot is charged to a job the
// scheduler is not tracking. That is legitimate for the length of a
// provision (bounded by MacOSProvisionTimeout, minutes) and illegitimate
// forever after — it is the exact signature of the 28-hour macOS outage,
// where `ephemerd status` reported active_jobs: 0 on a node whose only slot
// had been held since the previous afternoon. Pure so the escalation rule is
// testable without a real pool or a real wait.
func slotWaitSeverity(waited time.Duration, held, trackedJobs int) (slog.Level, string) {
	if held > 0 && trackedJobs == 0 && waited >= slotLeakSuspectAfter {
		return slog.LevelError, "blocked on a concurrency slot that no tracked job accounts for — suspected slot leak"
	}
	return slog.LevelWarn, "waiting for a free concurrency slot"
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
