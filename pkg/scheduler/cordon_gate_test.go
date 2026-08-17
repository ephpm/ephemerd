package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// Regression tests for issue #154: drain/cordon did not stop webhook-driven
// provisioning, so a node never quiesced.
//
// Observed on a live node:
//
//	23:52:29  drain --wait  ->  "Cordoned: daemon stopped claiming new jobs"
//	23:53:51  provisioning runner for job ...
//	23:53:51  registered repo-level JIT runner ...
//
// The cordon WAS checked in handleQueued. What it was not checked at was the
// point of provisioning. handleQueued admits a job and hands it to a
// provisioning path, which then blocks on the concurrency semaphore — on a
// max_concurrent = 1 node, for the whole duration of whatever is already
// running. The cordon lands during that wait. When the slot frees, the path
// resumes from a decision taken BEFORE the cordon and registers a JIT runner
// against a node the operator was told had stopped claiming.
//
// The fix makes the cordon a live gate at every point a dispatch can turn into
// a runner: handleQueued (admission), admitDispatch (post-semaphore), and
// claimJob (the hard backstop, since Provider.ClaimJob is the one call that
// registers a JIT runner).

// waitForPending (fungibility_test.go) blocks until the job is admitted by
// handleQueued — past the draining check and on its way to a provisioning
// path. That is the point after which the old code was committed to
// provisioning, so it is where these tests land the cordon.

// --- #154: the exact failure — cordon lands while a dispatch waits for a slot ---

// TestCordon_DispatchParkedOnSemaphoreProvisionsNothing reproduces the live
// failure. The single concurrency slot is held, so a webhook-admitted job is
// parked in handleLocalJob exactly where the production job was parked for 82
// seconds. The cordon then arrives. When the slot frees, nothing may be
// claimed and no runner may be registered.
func TestCordon_DispatchParkedOnSemaphoreProvisionsNothing(t *testing.T) {
	// claimErrorProvider counts claims and then fails them, so a regression
	// reports "claimed 1 job(s) after the cordon" instead of panicking deeper
	// in the provisioning path on the test's nil Runtime.
	prov := newClaimErrorProvider("github", errors.New("claim rejected by test"))
	s := New(Config{
		Providers:     []providers.Provider{prov},
		MaxConcurrent: 1,
		Log:           quietLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.bindContexts(ctx)
	s.webhookMode = true // webhook-driven fleet, as in the report

	// Occupy the only slot, the way a job already running would.
	s.sem <- struct{}{}

	event := providers.JobEvent{Provider: prov, Action: "queued", Repo: "r", JobID: 154}
	key := keyFor(event)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleQueued(ctx, event)
	}()

	// The job is admitted (cordon not yet issued) and blocks on the semaphore.
	if !waitForPending(t, s, key) {
		t.Fatal("job never reached the pending state; handleQueued did not admit it")
	}
	if got := prov.claims.Load(); got != 0 {
		t.Fatalf("job claimed %d time(s) before the slot was released; test is not exercising the parked-dispatch window", got)
	}

	// The operator cordons. This is the moment `drain --wait` reports
	// "Cordoned: daemon stopped claiming new jobs" and sets draining: true.
	s.Cordon()

	// The in-flight job finishes and frees the slot.
	<-s.sem

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parked dispatch never unwound after the slot was released")
	}

	if got := prov.claims.Load(); got != 0 {
		t.Errorf("cordoned scheduler claimed %d job(s) after the cordon; the node never quiesces (issue #154)", got)
	}

	s.mu.Lock()
	running := len(s.running)
	_, stillPending := s.pending[key]
	s.mu.Unlock()
	if running != 0 {
		t.Errorf("cordoned scheduler has %d running job(s), want 0", running)
	}
	if stillPending {
		t.Error("abandoned dispatch left a pending stamp; pending has no TTL, so the job would be blocked forever after Uncordon")
	}

	// The slot must be returned, or a cordoned-then-uncordoned node runs at
	// reduced capacity forever.
	select {
	case s.sem <- struct{}{}:
		<-s.sem
	default:
		t.Error("abandoned dispatch did not release its concurrency slot")
	}
}

// --- cordoned + webhook event provisions nothing ---

// TestCordon_WebhookEventProvisionsNothing is the headline assertion from the
// issue: a cordoned scheduler must provision NOTHING in response to a webhook
// event. Unlike the poll path there is no continuous re-delivery to fall back
// on, so this is the case that made the node keep taking work.
func TestCordon_WebhookEventProvisionsNothing(t *testing.T) {
	prov := newClaimCountingProvider("github")
	s := New(Config{
		Providers:     []providers.Provider{prov},
		MaxConcurrent: 4,
		Log:           quietLogger(),
	})
	s.bindContexts(context.Background())
	s.webhookMode = true

	s.Cordon()

	// Three distinct webhook deliveries, as a busy repo would produce.
	for _, jobID := range []int64{101, 102, 103} {
		s.handleQueued(context.Background(), providers.JobEvent{
			Provider: prov, Action: "queued", Repo: "r", JobID: jobID,
			Labels: []string{"self-hosted", "linux"},
		})
	}

	if got := prov.claims.Load(); got != 0 {
		t.Errorf("cordoned scheduler claimed %d job(s) from webhook events, want 0", got)
	}
	s.mu.Lock()
	running, pending := len(s.running), len(s.pending)
	s.mu.Unlock()
	if running != 0 {
		t.Errorf("cordoned scheduler has %d running job(s), want 0", running)
	}
	if pending != 0 {
		t.Errorf("cordoned scheduler left %d pending stamp(s); they have no TTL and would block those jobs after Uncordon", pending)
	}
}

// --- cordoned + poll path provisions nothing ---

// TestCordon_PollEventProvisionsNothing pins the poll/claim path (webhookMode
// false), which the issue suspected was already correct. It is asserted here so
// the two dispatch sources are held to one contract.
func TestCordon_PollEventProvisionsNothing(t *testing.T) {
	prov := newClaimCountingProvider("github")
	s := New(Config{
		Providers:     []providers.Provider{prov},
		MaxConcurrent: 4,
		Log:           quietLogger(),
	})
	s.bindContexts(context.Background())
	s.webhookMode = false

	s.Cordon()
	s.handleQueued(context.Background(), providers.JobEvent{
		Provider: prov, Action: "queued", Repo: "r", JobID: 201,
	})

	if got := prov.claims.Load(); got != 0 {
		t.Errorf("cordoned scheduler claimed %d job(s) from a poll event, want 0", got)
	}
}

// --- claimJob is the by-construction backstop ---

// TestCordon_ClaimJobRefusesRegardlessOfCaller pins the gate that makes the
// cordon hold by construction. claimJob is the sole funnel through which any
// path registers a JIT runner, so a future dispatch source that forgets the
// earlier gates still cannot put a runner on a cordoned node.
func TestCordon_ClaimJobRefusesRegardlessOfCaller(t *testing.T) {
	prov := newClaimCountingProvider("github")
	s := New(Config{Providers: []providers.Provider{prov}, Log: quietLogger()})
	s.bindContexts(context.Background())
	s.Cordon()

	event := providers.JobEvent{Provider: prov, Action: "queued", Repo: "r", JobID: 301}
	claim, err := s.claimJob(context.Background(), &event, nil, quietLogger(), 3)
	if !errors.Is(err, errCordoned) {
		t.Errorf("claimJob on a cordoned scheduler returned err = %v, want errCordoned", err)
	}
	if claim != nil {
		t.Error("claimJob returned a claim on a cordoned scheduler")
	}
	if got := prov.claims.Load(); got != 0 {
		t.Errorf("claimJob reached the provider %d time(s) while cordoned, want 0", got)
	}

	// ...and the same call succeeds once uncordoned, so the backstop is a
	// gate and not a permanent block.
	s.Uncordon()
	if _, err := s.claimJob(context.Background(), &event, nil, quietLogger(), 3); err != nil {
		t.Errorf("claimJob after Uncordon returned %v, want nil", err)
	}
	if got := prov.claims.Load(); got != 1 {
		t.Errorf("provider saw %d claim(s) after Uncordon, want 1", got)
	}
}

// TestCordon_ErrCordonedIsNotRetried pins that a cordon refusal is dropped by
// the retry queue rather than spinning its backoff ladder. Without this,
// errCordoned falls through to errUnknownRetryable and a cordoned node keeps a
// live retry schedule for work it has refused.
func TestCordon_ErrCordonedIsNotRetried(t *testing.T) {
	if got := classifyErr(errCordoned); got != errNonRetryable {
		t.Errorf("classifyErr(errCordoned) = %v, want %v", got, errNonRetryable)
	}
	// Wrapped, as it would arrive from a provisioning path.
	if got := classifyErr(errors.New("claiming job: " + errCordoned.Error())); got == errNonRetryable {
		// Sanity: the string form alone must not be what carries the class;
		// errors.Is on the sentinel is. This is informational only.
		t.Log("note: cordon error string also classifies non-retryable")
	}
}

// TestCordon_DoesNotEnqueueNewRetries pins that a cordoned scheduler takes on
// no NEW retry work. A retry scheduled during a drain fires minutes later
// against a node that is draining or, during an upgrade, already gone.
func TestCordon_DoesNotEnqueueNewRetries(t *testing.T) {
	prov := newClaimCountingProvider("github")
	s := New(Config{
		Providers: []providers.Provider{prov},
		Log:       quietLogger(),
		Retry:     RetryConfig{Enabled: true, Jitter: 0},
	})
	s.bindContexts(context.Background())
	event := providers.JobEvent{Provider: prov, Action: "queued", Repo: "r", JobID: 401}

	// Uncordoned: a retryable failure is enqueued as normal.
	s.enqueueRetryIfEligible(context.Background(), event, errors.New("503 service unavailable"))
	if got := s.retry.Len(); got != 1 {
		t.Fatalf("retry queue depth = %d before cordon, want 1", got)
	}

	// Cordoned: no new retry work is taken on.
	s.Cordon()
	s.enqueueRetryIfEligible(context.Background(), providers.JobEvent{
		Provider: prov, Action: "queued", Repo: "r", JobID: 402,
	}, errors.New("503 service unavailable"))
	if got := s.retry.Len(); got != 1 {
		t.Errorf("retry queue depth = %d after a cordoned enqueue, want 1 (no new work)", got)
	}
}

// --- cordon must not poison jobs it refuses ---

// TestCordon_DoesNotBurnZombieBudget pins that a cordon rejection is a refusal,
// not a failed provisioning attempt. The zombie guard permanently skips a job
// after maxProvisionAttempts; the draining check used to sit AFTER the counter
// was incremented, so a cordon held for maxProvisionAttempts * seenTTL (~50
// min) would mark every refused job undispatchable even after Uncordon.
func TestCordon_DoesNotBurnZombieBudget(t *testing.T) {
	// claimErrorProvider lets the post-Uncordon pass run all the way to
	// ClaimJob (proving the budget survived) without touching the nil Runtime.
	prov := newClaimErrorProvider("github", errors.New("claim rejected by test"))
	s := New(Config{Providers: []providers.Provider{prov}, Log: quietLogger()})
	s.bindContexts(context.Background())
	s.Cordon()

	event := providers.JobEvent{Provider: prov, Action: "queued", Repo: "r", JobID: 501}
	key := keyFor(event)

	// Many refusals, as a long cordon with a continuous poll would produce.
	for i := 0; i < maxProvisionAttempts*3; i++ {
		s.mu.Lock()
		delete(s.seen, key) // simulate the seen stamp aging out between polls
		s.mu.Unlock()
		s.handleQueued(context.Background(), event)
	}

	s.mu.Lock()
	attempts := s.attempts[key]
	s.mu.Unlock()
	if attempts != 0 {
		t.Errorf("cordon rejections consumed %d provision attempt(s); a long cordon would zombie the job permanently", attempts)
	}

	// Proof that the budget is intact: the job still provisions after Uncordon.
	s.Uncordon()
	s.mu.Lock()
	delete(s.seen, key)
	s.mu.Unlock()
	s.handleQueued(context.Background(), event)
	if got := prov.claims.Load(); got != 1 {
		t.Errorf("after Uncordon the job claimed %d time(s), want 1; the cordon poisoned it", got)
	}
}

// --- uncordon restores claiming through the same gate ---

// TestUncordon_RestoresDispatchGate checks the post-semaphore gate itself, the
// one that was missing. drain_test.go's TestUncordon_RestoresClaiming covers
// the admission gate; this covers admitDispatch so both ends of the cordon are
// pinned.
func TestUncordon_RestoresDispatchGate(t *testing.T) {
	s := New(Config{Log: quietLogger()})
	s.bindContexts(context.Background())
	s.webhookMode = true
	key := jobKey{Provider: "github", JobID: 601}

	if got := s.admitDispatch(key); got != dispatchAdmit {
		t.Fatalf("admitDispatch on a healthy scheduler = %v, want dispatchAdmit", got)
	}

	s.Cordon()
	if got := s.admitDispatch(key); got != dispatchAbandonCordoned {
		t.Errorf("admitDispatch while cordoned = %v, want dispatchAbandonCordoned", got)
	}

	s.Uncordon()
	if got := s.admitDispatch(key); got != dispatchAdmit {
		t.Errorf("admitDispatch after Uncordon = %v, want dispatchAdmit; claiming was not restored", got)
	}

	// The cordon must not mask the fungibility verdict it shares this gate
	// with: a job a sibling runner already ran is still abandoned as
	// satisfied, not misreported as cordoned.
	s.mu.Lock()
	s.started[key] = time.Now()
	s.mu.Unlock()
	if got := s.admitDispatch(key); got != dispatchAbandonSatisfied {
		t.Errorf("admitDispatch for an already-satisfied job = %v, want dispatchAbandonSatisfied", got)
	}
}

// TestUncordon_RestoresProvisioningEndToEnd walks a job all the way to a claim
// after Uncordon, so the restore is proven through the provisioning path and
// not just the flag.
func TestUncordon_RestoresProvisioningEndToEnd(t *testing.T) {
	// As in drain_test.go: the claim error aborts provisioning immediately
	// after ClaimJob, so the gate is proven without a real Runtime.
	prov := newClaimErrorProvider("github", errors.New("claim rejected by test"))
	s := New(Config{
		Providers:     []providers.Provider{prov},
		MaxConcurrent: 2,
		Log:           quietLogger(),
	})
	s.bindContexts(context.Background())
	s.webhookMode = true

	event := providers.JobEvent{Provider: prov, Action: "queued", Repo: "r", JobID: 701}
	key := keyFor(event)

	s.Cordon()
	s.handleQueued(context.Background(), event)
	if got := prov.claims.Load(); got != 0 {
		t.Fatalf("cordoned scheduler claimed %d job(s), want 0", got)
	}

	s.Uncordon()
	// The seen stamp from the refusal is deliberately left behind to age out
	// via seenTTL; expire it the way a later poll would observe it.
	s.mu.Lock()
	delete(s.seen, key)
	s.mu.Unlock()

	s.handleQueued(context.Background(), event)
	if got := prov.claims.Load(); got != 1 {
		t.Errorf("uncordoned scheduler claimed %d job(s), want 1; claiming was not restored", got)
	}
}

// --- running jobs are unaffected by the cordon ---

// TestCordon_RunningJobsUnaffected pins the other half of the contract: cordon
// means stop claiming NEW work, so jobs already running must continue to
// completion. A cordon that killed them would turn every drain and every
// upgrade into a job-losing event.
func TestCordon_RunningJobsUnaffected(t *testing.T) {
	prov := newClaimCountingProvider("github")
	s := New(Config{
		Providers:     []providers.Provider{prov},
		MaxConcurrent: 2,
		Log:           quietLogger(),
	})
	s.bindContexts(context.Background())
	s.webhookMode = true

	// Two jobs already running.
	keys := []jobKey{{Provider: "github", JobID: 801}, {Provider: "github", JobID: 802}}
	ctxs := make([]context.Context, 0, len(keys))
	for _, k := range keys {
		jobCtx, cancel := s.jobContext()
		defer cancel()
		ctxs = append(ctxs, jobCtx)
		s.running[k] = &runningJob{repo: "r", cancel: cancel, startedAt: time.Now()}
	}

	if got := s.Cordon(); got != 2 {
		t.Errorf("Cordon() = %d active jobs, want 2", got)
	}

	// A new job arrives and is refused...
	s.handleQueued(context.Background(), providers.JobEvent{
		Provider: prov, Action: "queued", Repo: "r", JobID: 803,
	})
	if got := prov.claims.Load(); got != 0 {
		t.Errorf("cordoned scheduler claimed %d new job(s), want 0", got)
	}

	// ...while the running jobs are untouched: still tracked, still counted
	// by ActiveJobs (which `drain --wait` polls), contexts still alive.
	s.mu.Lock()
	running := len(s.running)
	s.mu.Unlock()
	if running != 2 {
		t.Errorf("cordon removed running jobs: %d remain, want 2", running)
	}
	if got := s.ActiveJobs(); got != 2 {
		t.Errorf("ActiveJobs() = %d after cordon, want 2", got)
	}
	for i, jobCtx := range ctxs {
		if jobCtx.Err() != nil {
			t.Errorf("cordon cancelled running job %d's context: %v", i, jobCtx.Err())
		}
	}

	// As they complete, the active count trends to zero — which is exactly
	// what `drain --wait` blocks on and what never happened in #154.
	for _, k := range keys {
		s.mu.Lock()
		delete(s.running, k)
		s.mu.Unlock()
	}
	if got := s.ActiveJobs(); got != 0 {
		t.Errorf("ActiveJobs() = %d after the running jobs finished, want 0", got)
	}
}
