package scheduler

import (
	"context"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/providers"
	"github.com/ephpm/ephemerd/pkg/runnerbusy"
	"github.com/ephpm/ephemerd/pkg/runtime"
)

// The hard invariant this file enforces: NEVER DESTROY A RUNNER THAT IS
// EXECUTING A JOB.
//
// The orphan sweep's two rules (intent-keyed grace, label-set fungibility
// reconciliation) are good at spotting runners that have lost their
// purpose, but both decide from the same evidence: the scheduler's belief
// about which runner is bound to which job, assembled from in_progress
// webhooks. That belief is inference, and a same-label burst is exactly
// the load that makes it wrong — three JIT runners are dispatched, GitHub
// permutes the assignments, and between the first in_progress delivery
// and the last there is a window in which a runner that is already
// executing a build still looks unbound. A sweep landing in that window
// (every completed event triggers one) retires a live job.
//
// So the rules no longer decide. They NOMINATE, and a busy check taken at
// the moment of teardown either confirms the nomination or vetoes it. The
// check asks the runtime ephemerd owns — is there a Runner.Worker process
// in there? — and falls back to GitHub's per-runner busy flag when it
// cannot see the runner at all. Anything short of a positive "idle"
// answer is a veto.
//
// A veto that could never be overridden would trade one leak for
// another — a wedged runner would squat a concurrency slot forever — so
// the veto is bounded in time. See reapPolicy and decideReap.

const (
	// busyProbeTimeout bounds a single busy check. The local probes are
	// sub-millisecond; the bound exists so a wedged containerd shim or a
	// stalled GitHub request cannot hold the sweep (and, through it, the
	// scheduler mutex on the next phase) open.
	busyProbeTimeout = 10 * time.Second

	// maxJobRuntime is the longest a single job can legitimately run when
	// no job_timeout is configured — GitHub's own per-job ceiling for
	// self-hosted runners. Used to derive the hard bound below.
	maxJobRuntime = 6 * time.Hour

	// hardBoundMargin is how far past a job's own deadline a runner may
	// keep vetoing its own teardown before we call it wedged. A job that
	// exceeds JobTimeout has already had its context cancelled and its
	// runner torn down by the normal path, so a runner still claiming to
	// be busy this far out is not running anything we can save.
	hardBoundMargin = 30 * time.Minute
)

// reapPolicy is the pair of time bounds that keep the busy veto from
// becoming permanent.
type reapPolicy struct {
	// Grace is the orphan sweep's existing grace window. It doubles as
	// the ceiling on an UNKNOWN verdict: if we cannot determine whether a
	// runner is busy, we fall back to exactly the pre-veto behaviour
	// (destroy once it has been unbound this long) rather than holding
	// the slot indefinitely.
	Grace time.Duration

	// HardBound is the ceiling on a POSITIVE busy verdict, measured from
	// dispatch. Past it, teardown proceeds over the veto with a loud log
	// line: the runner is wedged, not working.
	HardBound time.Duration
}

// reapPolicy derives the veto's time bounds from the scheduler config.
func (s *Scheduler) reapPolicy() reapPolicy {
	grace := s.cfg.OrphanSweep.Grace
	if grace <= 0 {
		grace = defaultOrphanGrace
	}
	limit := s.cfg.JobTimeout
	if limit <= 0 {
		limit = maxJobRuntime
	}
	hard := limit + hardBoundMargin
	if hard < grace {
		// A grace window longer than the hard bound would let the weaker
		// (unknown) escape outlive the stronger (busy) one.
		hard = grace
	}
	return reapPolicy{Grace: grace, HardBound: hard}
}

// reapInput is everything decideReap needs. Kept free of scheduler state
// so the decision is a pure function of observable facts.
type reapInput struct {
	Runner       string
	DispatchedAt time.Time
	Now          time.Time
	Busy         runnerbusy.State
	Policy       reapPolicy
}

// Outcomes, also used as the `outcome` metric label.
const (
	outcomeReaped  = "reaped"
	outcomeVetoed  = "vetoed"
	outcomeEscaped = "escaped"
)

// reapDecision is decideReap's verdict on one nominated runner.
type reapDecision struct {
	// Destroy is whether teardown may proceed.
	Destroy bool

	// Escape is true when teardown proceeds DESPITE the busy check
	// failing to clear the runner. Always logged at warn level: it is the
	// only path by which this code can still kill a live job, and its
	// rate is the signal that the busy check has stopped working.
	Escape bool

	// Outcome is one of outcomeReaped / outcomeVetoed / outcomeEscaped.
	Outcome string

	// Reason is a short, stable, human-readable explanation for the log.
	Reason string
}

// decideReap turns a nomination plus a busy verdict into a teardown
// decision. Pure: same inputs, same answer, no clock and no I/O.
//
// The three verdicts carry different weight, and the escape hatches are
// sized accordingly.
//
//   - Idle is a positive observation that no job is executing. Teardown
//     proceeds immediately — this is what makes aggressive reaping safe
//     and what lets orphan_grace stop being load-bearing.
//
//   - Busy is a positive observation that a job IS executing. It vetoes
//     teardown up to HardBound (job deadline plus a margin). Nothing
//     legitimate is still running past that, so a runner that keeps
//     answering "busy" there is wedged and gets destroyed.
//
//   - Unknown means we could not determine either way — no probe on this
//     platform, a runtime that refused the query, an unreachable API. It
//     is treated as possibly-busy and vetoes teardown, but only up to
//     Grace, at which point we are back to the pre-veto behaviour: a
//     runner unbound for the whole grace window is destroyed.
//
// Why both escapes are measured in TIME rather than in consecutive failed
// probes: the sweep runs on every job completion as well as on a timer,
// so a probe-count escape fires within milliseconds on a busy node during
// exactly the same-label burst this whole change exists to survive, and
// effectively never on a quiet one. Wedged-ness is a property of
// duration, so duration is what bounds it.
func decideReap(in reapInput) reapDecision {
	age := in.Now.Sub(in.DispatchedAt)

	if age >= in.Policy.HardBound && in.Busy != runnerbusy.Idle {
		return reapDecision{
			Destroy: true,
			Escape:  true,
			Outcome: outcomeEscaped,
			Reason:  "wedged: still not idle past the hard bound",
		}
	}

	switch in.Busy {
	case runnerbusy.Idle:
		return reapDecision{
			Destroy: true,
			Outcome: outcomeReaped,
			Reason:  "verified idle: no worker process on the runner",
		}
	case runnerbusy.Busy:
		return reapDecision{
			Outcome: outcomeVetoed,
			Reason:  "vetoed: the runner is executing a job",
		}
	default:
		if age >= in.Policy.Grace {
			return reapDecision{
				Destroy: true,
				Escape:  true,
				Outcome: outcomeEscaped,
				Reason:  "busy state undeterminable for the whole grace window",
			}
		}
		return reapDecision{
			Outcome: outcomeVetoed,
			Reason:  "vetoed: busy state could not be determined",
		}
	}
}

// vetoBusyNominations is the gate in front of teardown.
//
// It takes the sweep's nominations, asks each one's runtime whether a job
// is executing on it, and returns only the ones cleared for destruction —
// already unhooked from the bookkeeping maps.
//
// Runs with s.mu RELEASED: the probes do I/O. That opens a window in
// which an in_progress webhook can arrive and bind a nominated runner, so
// reapRunnerLocked re-validates the ledger entry (same binding, still
// unbound) before unhooking anything. A runner that got bound in the
// window is silently kept, which is the correct answer.
func (s *Scheduler) vetoBusyNominations(noms []orphanNomination, policy reapPolicy) []orphanVictim {
	if len(noms) == 0 {
		return nil
	}
	ctx := context.Background()
	victims := make([]orphanVictim, 0, len(noms))

	for _, n := range noms {
		state, source := s.runnerBusy(ctx, n.rj)
		d := decideReap(reapInput{
			Runner:       n.name,
			DispatchedAt: n.rb.dispatchedAt,
			Now:          time.Now(),
			Busy:         state,
			Policy:       policy,
		})
		metrics.OrphanReapDecisions.WithLabelValues(state.String(), d.Outcome).Inc()

		if !d.Destroy {
			s.cfg.Log.Info("orphan sweep nomination vetoed",
				"runner", n.name,
				"dispatched_for_job", n.rb.intentKey.JobID,
				"busy_verdict", state.String(),
				"probe", source,
				"reason", d.Reason)
			continue
		}

		s.mu.Lock()
		v, ok := s.reapRunnerLocked(n.name, n.rb)
		s.mu.Unlock()
		if !ok {
			s.cfg.Log.Debug("orphan sweep nomination went stale before teardown",
				"runner", n.name, "detail", "bound or cleaned up while the busy check ran")
			continue
		}
		v.discharged = n.discharged
		v.escaped = d.Escape
		v.verdict = state.String()
		v.reason = d.Reason
		victims = append(victims, v)
	}
	return victims
}

// busyProber answers "is this runner executing a job right now?".
// Swapped out in tests; the production implementation is
// (*Scheduler).probeRunnerBusy.
type busyProber func(ctx context.Context, rj *runningJob) (runnerbusy.State, string)

// runnerBusy runs the configured prober, defaulting to the real one.
func (s *Scheduler) runnerBusy(ctx context.Context, rj *runningJob) (runnerbusy.State, string) {
	if s.busyProbe != nil {
		return s.busyProbe(ctx, rj)
	}
	return s.probeRunnerBusy(ctx, rj)
}

// probeRunnerBusy is the layered busy check.
//
//  1. Local introspection of the container / VM / process ephemerd owns.
//     Preferred: it is ground truth, needs no network, spends no API
//     budget, and no webhook delivery can defeat it.
//  2. The provider's own busy flag, for the paths where ephemerd cannot
//     see into the runner (currently: runners dispatched into the Linux
//     sidecar VM, whose containerd is behind the dispatch gRPC boundary).
//  3. Unknown — which the caller must treat as possibly busy.
//
// The second return value names the authority that answered, for logs.
func (s *Scheduler) probeRunnerBusy(ctx context.Context, rj *runningJob) (runnerbusy.State, string) {
	ctx, cancel := context.WithTimeout(ctx, busyProbeTimeout)
	defer cancel()

	state, source, err := s.probeLocalBusy(ctx, rj)
	if state != runnerbusy.Unknown {
		return state, source
	}
	if err != nil {
		s.cfg.Log.Debug("local busy probe could not answer",
			"runner", rj.runnerName(), "probe", source, "error", err)
	}

	reporter, ok := rj.provider.(providers.RunnerBusyReporter)
	if !ok || rj.claim == nil {
		return runnerbusy.Unknown, source
	}
	busy, err := reporter.RunnerBusy(ctx, rj.claim)
	if err != nil {
		s.cfg.Log.Warn("provider busy check failed; treating the runner as possibly busy",
			"runner", rj.runnerName(), "error", err)
		return runnerbusy.Unknown, source
	}
	if busy {
		return runnerbusy.Busy, "provider"
	}
	return runnerbusy.Idle, "provider"
}

// probeLocalBusy inspects whatever ephemerd itself owns for this job.
//
// Every branch either answers from a live process listing or returns
// Unknown with an explicit reason. None of them can return Idle by
// default, which is the property that makes the veto safe.
func (s *Scheduler) probeLocalBusy(ctx context.Context, rj *runningJob) (runnerbusy.State, string, error) {
	switch {
	case rj.env != nil && rj.env.Task != nil:
		// Linux: containerd task PIDs are host PIDs, read via /proc.
		// Windows: HCS proxies a process listing out of the Hyper-V
		// isolated container. Both live in pkg/runnerbusy behind build
		// tags; macOS hosts have no local container and get Unknown.
		st, err := runnerbusy.ContainerBusy(
			namespaces.WithNamespace(ctx, runtime.Namespace), rj.env.Task, s.cfg.Log)
		return st, "container", err

	case rj.macosVM != nil:
		st, err := s.macOSVMBusy(ctx, rj.macosVM)
		return st, "macos-vm", err

	case rj.dispatched != "":
		// The runner is a container inside the Linux sidecar VM. Its
		// containerd is reachable only through the dispatch gRPC service,
		// which has no process-introspection call, and the host has no
		// view into the guest's PID namespace. Explicitly unavailable
		// rather than silently idle: the provider busy flag is the
		// authority for this path.
		return runnerbusy.Unknown, "dispatched", runnerbusy.ErrUnsupported
	}

	return runnerbusy.Unknown, "none", runnerbusy.ErrUnsupported
}
