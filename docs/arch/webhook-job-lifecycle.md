# Webhook Job Lifecycle & Stranded-Job Self-Heal

> **Status: implemented.** See `pkg/scheduler/scheduler.go`
> (`reprovisionIfStranded`, the `started` set, `handleInProgress`,
> `handleCompleted`) and `pkg/scheduler/reprovision_test.go`.

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
