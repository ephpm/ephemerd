# Webhook Job Lifecycle & Stranded-Job Self-Heal

> **Status: implemented.** See `pkg/scheduler/scheduler.go`
> (`reprovisionIfStranded`, the `started` set, `handleInProgress`,
> `handleCompleted`) and `pkg/scheduler/reprovision_test.go`.
>
> **Superseded in part.** The self-heal described below fixed the *demand*
> side only and left a wasted dispatch behind every reassignment. See
> [Second fix: reconcile on the label set](#second-fix-reconcile-on-the-label-set-not-the-job-id)
> at the end of this document.

## Problem: fungible JIT runners strand jobs

ephemerd registers **one ephemeral JIT runner per queued job**. It does *not*
get to choose which job that runner runs. GitHub treats all self-hosted JIT
runners with matching labels as **fungible**: when a runner connects and
long-polls for work, GitHub hands it *any* queued job with matching labels —
not necessarily the one ephemerd brought it up for.

For a multi-job workflow (every matrix build, every fan-out), several
same-label jobs queue at once. The runner dispatched "for" job A routinely ends
up running sibling job B. You see this in the logs as:

```
runner picked up a different job than it was dispatched for
```

This is normally harmless — the assignments just permute, and if there are N
runners for N jobs, all N run. It becomes a **stranding** bug because of how
ephemerd used to decide a job was "handled":

- `handleQueued` marks a job **`seen`** the instant its `queued` webhook
  arrives, and provisions a runner. That `seen` entry is an *optimistic bet*:
  "the runner I'm bringing up will run this job." It suppresses re-processing
  of that job for `seenTTL` (10 min).
- But the bet can lose. If runner-for-A instead runs B and exits, and A's own
  runner never materialized (claim error, crash, timing), then **A is still
  queued on GitHub, marked `seen` locally, and nothing brings up another
  runner for it.** A strands until the `seen` entry ages out (~10 min) or a
  low-frequency reconcile poll happens to sweep it.

Critically, **GitHub does not help here.** Webhook redelivery only fires on
*delivery failure* (non-2xx / timeout). Once ephemerd returns `200` to the
`queued` webhook, GitHub considers it delivered and never re-notifies just
because the job is still queued. Silent stranding is invisible to GitHub — the
onus is entirely on ephemerd to notice and recover.

## Root cause

The system keyed "job handled" on **dispatch intent** (we brought up a runner
for it) rather than **observed execution** (we saw it actually start). Dispatch
intent is a guess; fungibility breaks the guess.

## Fix: observed-state, event-driven re-provisioning

Two changes, both driven purely by webhooks ephemerd already receives.

### 1. A job is "satisfied" only when observed running

We track a `started` set (`map[jobKey]time.Time`) recording jobs we have
**observed** transition to `in_progress` or `completed`. This is the true
satisfaction signal, and it is **keyed on the job, not the runner** — so a job
that ran on a fungibly-reassigned sibling runner, or even on a *peer daemon's*
runner, still counts as satisfied (webhook `workflow_job` events fire
repo-wide, not per-runner).

- `handleInProgress` records `started[job]` — the job actually started
  somewhere.
- `handleCompleted` records `started[job]` too — covers the
  cancelled-while-queued shape (`queued -> completed` with no `in_progress` at
  all) and any missed `in_progress` delivery.

### 2. Re-provision on runner exit if the job never ran

Every runner's wait-goroutine (linux dispatch, macOS VM, native macOS, local
containerd) calls `reprovisionIfStranded` when the runner exits. If the job it
was dispatched for was **never observed running** (`started[key]` unset), the
job never actually ran — so we clear its `seen` dedup and re-dispatch it
immediately via `handleQueued`.

This is the whole self-heal. The trigger is the runner-exit event we already
have; the decision is "did the thing I was responsible for actually happen?";
the recovery reuses the normal dispatch path. **No polling, nothing lost.**

```
queued(A)         -> provision runner N_A; A NOT yet satisfied
queued(B)         -> provision runner N_B
in_progress(B,N_A)-> started[B]=now         (N_A ran B, a sibling — fungible)
                     (N_B failed to come up; A never assigned)
N_A exits         -> reprovisionIfStranded(A): started[A] unset -> re-dispatch A
queued(A) again   -> provision runner N_A2
in_progress(A,N_A2)-> started[A]=now         (A finally runs)
```

Contrast the non-stranding swap, which must *not* re-provision:

```
in_progress(B,N_A), in_progress(A,N_B)  -> started[A] and started[B] both set
N_A exits -> reprovisionIfStranded(A): started[A] set -> no-op
N_B exits -> reprovisionIfStranded(B): started[B] set -> no-op
```

### Guards

`reprovisionIfStranded` short-circuits when:

- **not in webhook mode** — `in_progress`/`completed` events (which set
  `started`) are only observable via webhooks; in poll mode "never started" is
  meaningless and the continuous poll already reconciles stranded jobs.
- **`started[key]` set** — the job ran (ours, a sibling's, or a peer's).
- **`running`/`pending[key]` set** — already being (re-)handled.
- **`attempts[key]` over the zombie cap** — a genuinely undispatchable job
  (e.g. a superseded workflow run GitHub keeps listing as queued but never
  dispatches) would otherwise re-provision on every runner exit forever.
- **draining** — shutting down.

Re-dispatch is launched as `go s.handleQueued(...)` so it never blocks on the
concurrency slot the exiting wait-goroutine is about to release, and it uses
the scheduler **root context** (`runCtx`), not the wait-goroutine's captured
`ctx` — the captured ctx may carry a stale `retryAttemptCtxKey` marker from the
original claim-retry path, which would misroute a re-claim failure.

## Interactions

- **Zombie cap (`maxProvisionAttempts = 5`).** `handleQueued` increments
  `attempts[key]` on every pass and bails before claiming once it exceeds the
  cap. Since re-provisioning re-enters `handleQueued`, a perpetually-stranded
  job consumes one attempt per runner exit and converges on the cap instead of
  looping. For a *true zombie* (superseded run whose runner comes up but is
  never assigned a job), the runner doesn't exit on its own — it's the
  **orphan sweep** (10-min grace) that reaps it, so zombie detection stays
  ~orphan-sweep-paced (~50 min total), unchanged.
- **Orphan sweep (`sweepOrphanRunners`).** Destroys runners dispatched but
  never bound within a grace window. Complementary: the sweep reaps the idle
  *runner*; `reprovisionIfStranded` re-provisions the stranded *job*. When the
  sweep kills a runner, its wait-goroutine unblocks and runs the self-heal
  check like any other exit.
- **`seen` dedup.** Still the first-line duplicate filter for redelivered /
  concurrent `queued` events. Re-provisioning deliberately clears the specific
  `seen[key]` so `handleQueued` acts, while leaving `attempts` intact for the
  zombie cap.
- **Retry queue.** Orthogonal — it handles *claim/provision failures*
  (rate-limit, transient 5xx) via a backoff ladder. The self-heal handles
  *successful dispatch that ran the wrong job*.
- **`started` pruning.** `cleanSeen` prunes `started` with a TTL of
  `JobTimeout + seenTTL` (or 6 h when no `JobTimeout` is set — GitHub's max job
  runtime). It must outlive the *longest* runner in a cohort: a sibling runner
  dispatched for a quick job X but assigned a long job doesn't exit — and thus
  can't check `started[X]` — until its long job finishes, up to `JobTimeout`
  after dispatch. Pruning `started[X]` earlier would let that late exit falsely
  re-provision the already-run X.

## The reconcile poll is now a backstop only

The periodic catch-up poll (`runReconcileLoop`, `[webhook] reconcile_interval`)
was originally the stranding remedy. With event-driven re-provisioning handling
the common case *instantly*, the poll is demoted to a **last-resort backstop
for genuinely dropped webhook deliveries** — network/tunnel loss where even
GitHub's delivery retry didn't reach us while we were up. That's the one case
the event-driven path can't see (no event arrives to react to). Its default
interval was raised `5m -> 30m`; set `reconcile_interval = "0s"` to disable it
entirely and rely purely on the event-driven path plus GitHub's delivery
retries.

## Why not just poll?

Polling treats the symptom. It recovers a stranded job only *after* its `seen`
entry ages out (the re-emitted `queued` is otherwise deduped), so worst-case
latency is ~10-15 min, and it costs an API list-call per interval. The
event-driven path recovers within seconds of the runner exit, costs nothing
extra, and — because it keys on observed execution rather than a poll snapshot
— cannot double-provision a job that actually ran. Polling remains only for the
residual case event-driven logic structurally cannot cover: a delivery that
never arrived.

## Second fix: reconcile on the label set, not the job id

The self-heal above fixed the **demand** side — a job stranded by fungible
reassignment gets re-dispatched instead of waiting out `seenTTL`. It never
touched the **supply** side, and that is where the cost moved.

Observed on the `max_concurrent = 1` macOS node: **266 orphan destroys** in
`/var/log/ephemerd.log`, **50 orphans against 222 completions** in the last
2000 lines (~18% of dispatches wasted), producing queue waits of **1–2.5 hours
for jobs that run in 1–3 minutes**.

### Mechanism

`handleQueued` accepts a job and hands it to a provisioning path, which then
blocks on the concurrency semaphore — at `max_concurrent = 1`, for the entire
duration of whatever is already running. While it is blocked, GitHub hands that
same job to an **already-dispatched** same-label runner, which runs it and
exits. Nothing between "slot acquired" and "claim runner" consulted `started`,
so when the slot finally freed, the path went on to register a fresh JIT runner
**for a job that had already finished**.

That runner is never assigned anything, so no `completed` event ever names it
and no teardown path touches it. It holds the pool's only concurrency slot
until the orphan sweep's grace window (`orphan_grace`, 90m in production)
expires — then the sweep destroys it, its wait-goroutine fires
`reprovisionIfStranded`, and the cycle repeats. Log timestamps
15:24:48 → 15:44:48 → 16:59:48 → 17:19:48 are consecutive turns of that loop.

The v0.1.4 self-heal made this *worse-shaped*, not better: it converted "job
strands for ~10 min" into "job re-dispatches promptly, and the surplus runner
orphans for the full grace window". Fewer stranded jobs, same wasted
dispatches — and on a single-slot node the wasted dispatch is the binding
constraint.

### Root cause

Dispatch accounting balanced on the **job id**. But a JIT runner is not bound
to a job id — GitHub hands it any queued job whose labels it satisfies. The
unit that must balance is the **label set**: N queued jobs sharing a label set
are served by N runners of that class, in whatever permutation GitHub picks.

### Fix

`labelSetKey(labels)` canonicalizes a job's labels (lowercased, trimmed,
sorted, deduped) into a **fungibility class**. `Scheduler.jobLabels` records the
class of every accepted job; `runnerBinding.labelSet` records the class each
dispatched runner serves.

1. **`admitDispatch(key)`** — the last gate before claiming, called by all four
   provisioning paths immediately after they acquire their concurrency slot.
   It returns false when the job was observed running while the handler sat
   blocked, so the dispatch is *discharged by the sibling's execution* instead
   of becoming an orphan. The caller releases its slot, keeping
   `max_concurrent` accounting correct.

2. **Label-set reconciliation in `sweepOrphanRunners`** — an unbound runner
   whose intent job ran elsewhere is a *spare*. Spares are allocated against
   their class's uncovered queued jobs (newest first, most grace remaining);
   surplus spares are retired immediately rather than waiting out the grace
   window. `handleCompleted` triggers the reconciliation so it is event-driven
   rather than waiting for the 5-minute sweep tick.

The intent-keyed grace rule is **unchanged and still required**: a runner
nothing ever wanted has no discharge signal, so only the grace window can reap
it. Rule 3 is additive.

### Deliberately conservative

There is **no** "already observed running" early-out in `handleQueued`. The
check lives only in `admitDispatch`, at the point of claiming. A fresh `queued`
event is always allowed back into the pipeline: if the platform says a job is
queued it may genuinely need a runner again, and rejecting it that early would
suppress it for the whole `started` TTL (`JobTimeout + seenTTL`, or 6h when no
job timeout is configured). Costing a briefly-held concurrency slot is the
cheaper mistake.

An idle spare does **not** suppress a pending dispatch for a *different*
same-label job. Doing so would be the fully-balanced model, but it bets the
second job's execution on GitHub assigning it to the spare; if that bet loses,
the job strands with nothing left to re-dispatch it. Retiring surplus spares
after the fact is strictly safe — it can waste a dispatch, never lose a job.

### Interaction with the zombie cap

A job over `maxProvisionAttempts` is not counted as class demand. We have given
up on it, so it must not keep a spare runner alive indefinitely.

## Third fix: never destroy a runner that is executing a job

The two fixes above made the sweep's *rules* correct. They did not make its
*evidence* correct, and that is what was killing live builds.

Observed 2026-08-12 on the `linux-amd64` pool: three `dind-test` jobs with
identical labels, dispatched concurrently, and a runner retired out from under
an in-flight build.

### Mechanism

"Unbound" was inferred from webhooks and never verified. `runnerBinding.bound`
flips only when `handleInProgress` processes an `in_progress` delivery naming
that runner. Until then the runner looks idle in the ledger no matter what it
is actually doing.

That is not a rare, dropped-delivery failure — it is the normal shape of a
same-label burst:

1. Three same-label jobs A, B, C queue. ephemerd dispatches runners rA, rB, rC
   (intent keys A, B, C). All three intent keys are now in `served`, so class
   demand is **0**.
2. GitHub permutes the assignments — say rA→B, rB→C, rC→A. Each runner starts
   executing immediately.
3. The three `in_progress` deliveries arrive over three separate HTTP requests
   and are processed in whatever order they land. The **first** one processed
   (job A, naming rC) sets `started[A]` and binds rC.
4. `started[A]` is exactly the discharge signal for **rA**, whose intent key is
   A. rA is now: unbound (its own `in_progress`, for job B, has not arrived),
   discharged, and in a class with zero demand. That is rule 2, and rule 2
   fires **immediately** — no grace window involved.
5. Any sweep landing in that window retires rA. `handleCompleted` calls
   `sweepOrphanRunners` on **every** completion, so it lands there routinely.

rA was executing job B. The window is the inter-delivery skew between two
`in_progress` webhooks of the same burst — hundreds of milliseconds is enough,
and no delivery has to be dropped, delayed or reordered for it to open. Rule 1
kills the same runner more slowly whenever a delivery genuinely *is* lost: the
grace window expires and the runner still looks unbound.

### Fix: the rules nominate, a busy check vetoes

Both rules now produce **nominations**. Nothing is unhooked from the
bookkeeping maps until a check taken *at the moment of teardown* — not derived
from event history — confirms the runner is not executing a job. The hard
invariant is: **never destroy a runner that is executing a job.**

The check is layered (`pkg/scheduler/busy.go`, `pkg/runnerbusy`):

1. **Local introspection (primary).** The actions-runner forks a
   `Runner.Worker` child only while a job is executing — the listener process
   is alive for the runner's whole life, so "a runner process exists" is not
   the signal; the worker is. ephemerd owns the runtime, so it can look:
   - **Linux (containerd):** `task.Pids()` → read `argv[0]` (falling back to
     `comm`) from `/proc` on the host.
   - **Windows (HCS / Hyper-V isolated):** the guest's processes are invisible
     to the host process table, so HCS's own `ProcessList()` is used — the same
     handle `pkg/metrics` already opens per container — matching `ImageName`.
   - **macOS VM:** `pgrep -x Runner.Worker` over the existing per-job SSH
     channel.
   - **Native macOS:** `pgrep -g <pgid> -x Runner.Worker` against the runner's
     process group on the host.
   - **Dispatched into the Linux sidecar VM:** explicitly **unavailable** —
     that containerd is behind the dispatch gRPC boundary and the host has no
     view into the guest's PID namespace. It degrades to layer 2 rather than
     silently answering "not busy".
2. **The provider's busy flag (secondary).** GitHub reports `busy` per runner.
   Consulted only when the local probe cannot answer, and only for a runner
   already nominated — one GET per nomination, off every hot path.
3. **Unknown.** Anything else. Unknown is *not* idle: it means possibly busy.

The probes run with `s.mu` released (they do I/O), which opens a window for an
`in_progress` delivery to bind a nominated runner. `reapRunnerLocked`
re-validates the ledger entry — same binding, still unbound — before unhooking
anything, so a runner bound during the probe survives its own nomination.

### Escape hatches, and why both are time-based

A veto that could never be overridden trades one leak for another: a wedged
runner would squat a concurrency slot forever. Two bounds, both logged at warn
level with an `ESCAPE:` prefix, and both counted in
`ephemerd_orphan_reap_decisions_total{outcome="escaped"}`:

- **Unknown → the grace window.** If the busy state cannot be determined for
  the whole grace window, teardown proceeds. This is exactly the pre-veto
  behaviour, so a platform with no usable probe is never *worse* than before —
  and rule 1's nominations, which are already past the grace window, keep their
  original timing on such a platform.
- **Busy → the hard bound.** A positive busy verdict is overridden only past
  `job_timeout + 30m` (or GitHub's 6h per-job ceiling + 30m when no job timeout
  is configured). A job that exceeds `job_timeout` has already had its context
  cancelled and its runner torn down by the normal path, so a runner still
  claiming to be busy out there is wedged, not working.

Both escapes are measured in **elapsed time**, not in consecutive failed
probes. The sweep runs on every job completion as well as on a timer, so a
probe-count escape would fire within milliseconds on a busy node — during
exactly the same-label burst this change exists to survive — and effectively
never on a quiet one. Wedged-ness is a property of duration, so duration is
what bounds it.

### Consequence: `orphan_grace` stops being load-bearing

`orphan_grace` existed to paper over precisely this uncertainty. Too short
killed live work; too long squatted a concurrency slot. With a verified-idle
answer, reaping is immediate and safe, and with a verified-busy answer no
window length can kill a build. The knob now only governs how long an
*undeterminable* runner is held — the fallback path — so per-pool tuning
(2m here, 15m there, 90m in the original incident) is no longer needed on any
pool whose runners are locally probeable.

The same veto answers the cross-daemon case: two pools both claiming arm64,
GitHub binds one, and the loser's runner squats. "Is it actually busy?" is the
right question there too, and the loser answers "no" immediately instead of
waiting out a window.
