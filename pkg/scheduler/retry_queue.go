// Package scheduler  -  retry_queue.go: in-memory claim/provision retry queue.
//
// GitHub does not re-deliver workflow_job webhooks. When ephemerd's initial
// claim attempt fails for a *retryable* reason (rate-limit exhaustion,
// transient network error, 5xx), the queued job is dropped forever unless
// something re-tries it. Before this queue existed, we lost jobs whenever
// the installation-token budget was exhausted at the moment a webhook
// fired; humans then noticed and re-queued each job by hand.
//
// This file implements a single-goroutine retry queue keyed by jobKey.
// Failed claims are re-scheduled on a jittered backoff ladder
// (30s, 1m, 2m, 5m, 10m by default), giving up after RetryConfig.MaxAge.
// When the GitHub rate snapshot says remaining == 0 with a known reset
// time, the next attempt is snapped to just after reset instead of
// falling blindly through the ladder  -  this avoids wasting attempts
// on a budget that is provably exhausted.
//
// Non-retryable failures (404 job gone, label mismatch, validation) are
// dropped immediately without enqueue.
//
// Completed webhooks call retryQueue.Drop(key) to cancel outstanding
// retries  -  otherwise a job that completed on a peer daemon would keep
// getting reattempted here until MaxAge.

package scheduler

import (
	"container/heap"
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
	gh "github.com/google/go-github/v72/github"
)

// RetryConfig tunes the claim retry queue.
//
// A zero-valued RetryConfig disables retries entirely, matching the
// pre-existing "log-and-drop" behavior. In practice New() applies
// sensible defaults so callers get retries by default.
type RetryConfig struct {
	// Enabled toggles the entire retry queue. When false, failures still
	// log and drop as before (no behavior change).
	Enabled bool

	// Schedule is the ordered backoff ladder. Each entry is the base
	// delay for that attempt; jitter is applied on top. If nil, defaults
	// to {30s, 1m, 2m, 5m, 10m}.
	Schedule []time.Duration

	// MaxAge is the wall-clock budget from first failure to giving up.
	// Once (now - firstFailure) > MaxAge, we log a WARN and drop. Default 90m.
	MaxAge time.Duration

	// Jitter is the fraction (0-1) of a delay that's randomized +/- around
	// the base value. Set to a NEGATIVE value (e.g. -1) to request the
	// default (0.2 = +/-20%). Literal 0 is honored  -  tests use it for
	// deterministic scheduling.
	Jitter float64

	// RateHint returns the last-observed GitHub rate-limit state. When
	// remaining == 0 and now < reset and updated is fresh (<5m old),
	// the next attempt is snapped to reset + a small jitter instead of
	// the Schedule entry. Nil-safe: nil means "no rate awareness".
	RateHint func() (remaining int64, reset time.Time, updated time.Time)

	// Now is the clock function. Defaults to time.Now. Tests inject
	// a fake clock so backoff scheduling is deterministic.
	Now func() time.Time
}

// defaultRetrySchedule is the fallback backoff ladder  -  total ~18m
// before the caller runs out of ladder entries; MaxAge caps overall
// budget at 90m by default (repeating the last entry after that).
var defaultRetrySchedule = []time.Duration{
	30 * time.Second,
	1 * time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
}

// applyDefaults fills in zero fields with sensible defaults. Called once
// by newRetryQueue.
func (c *RetryConfig) applyDefaults() {
	if len(c.Schedule) == 0 {
		c.Schedule = defaultRetrySchedule
	}
	if c.MaxAge <= 0 {
		c.MaxAge = 90 * time.Minute
	}
	// Jitter defaults only apply when explicitly requested via a
	// negative value (sentinel); literal 0 disables jitter. Values > 1
	// are clamped to 0.2 as a safety net so a misconfigured knob doesn't
	// produce nonsense delays.
	switch {
	case c.Jitter < 0:
		c.Jitter = 0.2
	case c.Jitter > 1:
		c.Jitter = 0.2
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// retryHandler is the callback invoked when a retry fires. Returns nil
// on success (drops the retry). Any error triggers another schedule
// step, unless classifyErr says it's non-retryable.
type retryHandler func(ctx context.Context, event providers.JobEvent) error

// retryItem is an entry in the priority queue.
type retryItem struct {
	key          jobKey
	event        providers.JobEvent
	handler      retryHandler
	attempts     int       // number of prior failed attempts
	firstFailure time.Time // when the FIRST attempt failed (for MaxAge)
	nextAttempt  time.Time // when to fire next
	index        int       // heap index; -1 when not in the heap
}

// retryHeap implements heap.Interface, ordered by nextAttempt ascending.
type retryHeap []*retryItem

func (h retryHeap) Len() int            { return len(h) }
func (h retryHeap) Less(i, j int) bool  { return h[i].nextAttempt.Before(h[j].nextAttempt) }
func (h retryHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *retryHeap) Push(x any) {
	it := x.(*retryItem)
	it.index = len(*h)
	*h = append(*h, it)
}
func (h *retryHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	it.index = -1
	*h = old[:n-1]
	return it
}

// retryQueue is a single-goroutine, priority-heap-driven retry loop.
type retryQueue struct {
	cfg RetryConfig
	log *slog.Logger

	mu    sync.Mutex
	heap  retryHeap
	index map[jobKey]*retryItem // for O(1) Drop / update

	wake chan struct{} // signals the loop that heap head changed
}

// newRetryQueue constructs a queue. Call Run to start the drain loop.
func newRetryQueue(cfg RetryConfig, log *slog.Logger) *retryQueue {
	cfg.applyDefaults()
	return &retryQueue{
		cfg:   cfg,
		log:   log,
		index: make(map[jobKey]*retryItem),
		wake:  make(chan struct{}, 1),
	}
}

// Enabled reports whether the queue will actually schedule retries.
func (q *retryQueue) Enabled() bool { return q != nil && q.cfg.Enabled }

// signal wakes the drain loop. Non-blocking; a coalesced wake is fine.
func (q *retryQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Add enqueues (or refreshes) a retry for the given event. Called by
// the scheduler when a claim/provision path fails with a retryable
// error. If the queue is disabled or the error is non-retryable, this
// is a no-op after logging.
func (q *retryQueue) Add(event providers.JobEvent, handler retryHandler, err error) {
	if q == nil || !q.cfg.Enabled {
		return
	}
	class := classifyErr(err)
	if class == errNonRetryable {
		q.log.Debug("retry queue: not enqueuing non-retryable error",
			"job_id", event.JobID, "provider", providerName(event), "error", err)
		return
	}

	key := keyFor(event)
	now := q.cfg.Now()

	q.mu.Lock()
	defer q.mu.Unlock()

	it, existed := q.index[key]
	if !existed {
		it = &retryItem{
			key:          key,
			event:        event,
			handler:      handler,
			firstFailure: now,
			index:        -1,
		}
		q.index[key] = it
	}
	it.attempts++

	// Give-up check: too much wall-clock time has passed since the
	// first failure.
	if now.Sub(it.firstFailure) > q.cfg.MaxAge {
		q.log.Warn("retry queue: giving up on job  -  max age exceeded",
			"job_id", event.JobID,
			"provider", providerName(event),
			"attempts", it.attempts,
			"age", now.Sub(it.firstFailure))
		q.removeLocked(it)
		return
	}

	// Pick the delay: rate-aware if applicable, otherwise ladder + jitter.
	delay := q.nextDelayLocked(it, class)
	it.nextAttempt = now.Add(delay)

	if it.index < 0 {
		heap.Push(&q.heap, it)
	} else {
		heap.Fix(&q.heap, it.index)
	}
	q.signal()

	q.log.Info("retry queue: scheduled retry",
		"job_id", event.JobID,
		"provider", providerName(event),
		"attempt", it.attempts,
		"delay", delay,
		"error_class", class)
}

// Drop removes any pending retry for the given key. Called from
// handleCompleted / on successful claim / on in_progress webhook so we
// don't keep retrying jobs that were picked up elsewhere.
func (q *retryQueue) Drop(key jobKey) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if it, ok := q.index[key]; ok {
		q.removeLocked(it)
	}
}

// Len reports the current queue depth (for metrics and tests).
func (q *retryQueue) Len() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.heap)
}

// removeLocked pulls an item out of both heap and index. Caller holds mu.
func (q *retryQueue) removeLocked(it *retryItem) {
	if it.index >= 0 {
		heap.Remove(&q.heap, it.index)
	}
	delete(q.index, it.key)
}

// nextDelayLocked returns the delay until the next attempt, jitter
// applied and (optionally) snapped to a rate-limit reset. Caller holds mu.
func (q *retryQueue) nextDelayLocked(it *retryItem, class errClass) time.Duration {
	// Rate-limit awareness: only when we're actually retrying because of
	// a rate error AND the hint is fresh. Otherwise a 5xx spike would
	// pile up behind a distant reset, which is not what we want.
	if class == errRateLimit && q.cfg.RateHint != nil {
		remaining, reset, updated := q.cfg.RateHint()
		now := q.cfg.Now()
		if remaining == 0 && !reset.IsZero() && reset.After(now) &&
			!updated.IsZero() && now.Sub(updated) < 5*time.Minute {
			// Sleep until just after reset, with a small [0, 20s)
			// jitter so multiple daemons sharing the token don't
			// stampede at the exact same second.
			jitterMs := int64(20_000)
			off := time.Duration(rand.Int64N(jitterMs)) * time.Millisecond
			return reset.Sub(now) + 5*time.Second + off
		}
	}

	// Ladder + jitter. attempts is 1 on the first retry; index 0 into
	// the schedule is that first delay.
	idx := it.attempts - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(q.cfg.Schedule) {
		idx = len(q.cfg.Schedule) - 1
	}
	base := q.cfg.Schedule[idx]
	return jittered(base, q.cfg.Jitter)
}

// jittered applies +/-frac jitter to base. Split out for testability.
func jittered(base time.Duration, frac float64) time.Duration {
	if frac <= 0 {
		return base
	}
	span := float64(base) * frac
	// Range: [-span, +span).
	off := (rand.Float64()*2 - 1) * span
	d := time.Duration(float64(base) + off)
	if d < 0 {
		d = 0
	}
	return d
}

// Run drives the drain loop until ctx is cancelled. Safe to call once
// per queue. If the queue is disabled, Run just blocks on ctx.
func (q *retryQueue) Run(ctx context.Context) {
	if q == nil || !q.cfg.Enabled {
		<-ctx.Done()
		return
	}

	// A single timer we re-arm every iteration. Nil means "no head".
	var timer *time.Timer
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
	}
	defer stopTimer()

	for {
		q.mu.Lock()
		var next time.Duration = -1
		if len(q.heap) > 0 {
			d := q.heap[0].nextAttempt.Sub(q.cfg.Now())
			if d < 0 {
				d = 0
			}
			next = d
		}
		q.mu.Unlock()

		stopTimer()
		if next >= 0 {
			timer = time.NewTimer(next)
		}

		var timerCh <-chan time.Time
		if timer != nil {
			timerCh = timer.C
		}

		select {
		case <-ctx.Done():
			return
		case <-q.wake:
			// Heap changed; loop and re-check head.
			continue
		case <-timerCh:
			q.fireDue(ctx)
		}
	}
}

// fireDue pops all items whose nextAttempt is <= now and runs their
// handlers. Each handler runs in its own goroutine so a slow handler
// doesn't block the timer loop; on failure the handler is expected to
// call Add again with the new error.
func (q *retryQueue) fireDue(ctx context.Context) {
	now := q.cfg.Now()
	var due []*retryItem

	q.mu.Lock()
	for len(q.heap) > 0 && !q.heap[0].nextAttempt.After(now) {
		it := heap.Pop(&q.heap).(*retryItem)
		// Intentionally DO NOT delete(q.index, it.key) here: leaving the
		// popped item registered lets a failure re-Add (via runOne) find
		// it and advance attempts/preserve firstFailure, rather than
		// building a fresh item that resets the ladder and MaxAge.
		due = append(due, it)
	}
	q.mu.Unlock()

	for _, it := range due {
		go q.runOne(ctx, it)
	}
}

// runOne invokes the handler once, and on failure re-adds via Add so the
// backoff ladder advances. On success we do nothing  -  the handler took
// over lifecycle tracking and any future completed webhook will Drop
// the (already-absent) key harmlessly.
func (q *retryQueue) runOne(ctx context.Context, it *retryItem) {
	q.log.Info("retry queue: firing attempt",
		"job_id", it.event.JobID,
		"provider", providerName(it.event),
		"attempt", it.attempts+1)
	err := it.handler(ctx, it.event)
	if err == nil {
		// fireDue leaves the popped item in q.index so a failure re-Add
		// advances the ladder; on success nothing re-Adds it, so drop the
		// lingering index entry here. removeLocked tolerates index < 0.
		q.Drop(it.key)
		return
	}
	// Re-enqueue on failure. Add() decides retry/give-up.
	q.Add(it.event, it.handler, err)
}

// --- error classification ---

type errClass int

const (
	errNonRetryable errClass = iota
	errRateLimit             // 429 / 403 with rate headers
	errServerSide            // 5xx
	errNetwork               // network/DNS/timeout
	errUnknownRetryable      // last-resort: probably safe to retry
)

func (c errClass) String() string {
	switch c {
	case errNonRetryable:
		return "non_retryable"
	case errRateLimit:
		return "rate_limit"
	case errServerSide:
		return "server_side"
	case errNetwork:
		return "network"
	case errUnknownRetryable:
		return "unknown_retryable"
	default:
		return "unknown"
	}
}

// classifyErr sorts a claim/provision error into a retry class. Kept
// public within the package so scheduler.go can decide whether to log
// vs enqueue even before calling Add.
func classifyErr(err error) errClass {
	if err == nil {
		return errNonRetryable
	}

	// GitHub-specific rate limit errors  -  these are ALWAYS retryable.
	var rle *gh.RateLimitError
	if errors.As(err, &rle) {
		return errRateLimit
	}
	var arle *gh.AbuseRateLimitError
	if errors.As(err, &arle) {
		return errRateLimit
	}

	// GitHub ErrorResponse: status-code driven.
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		switch ghErr.Response.StatusCode {
		case http.StatusNotFound, http.StatusUnprocessableEntity,
			http.StatusUnauthorized, http.StatusBadRequest,
			http.StatusForbidden:
			// 403 without RateLimitError = permission problem, not rate.
			// Rate-limit 403s are already caught by the RateLimitError
			// branch above.
			// 404 job gone: definitely don't retry.
			// 401/400/422 validation: fixing requires config change,
			// not time.
			// (Conflict 409 is handled inside claimJob by name-retry.)
			return errNonRetryable
		case http.StatusTooManyRequests:
			return errRateLimit
		}
		if ghErr.Response.StatusCode >= 500 {
			return errServerSide
		}
	}

	// Bare network errors  -  check via net.Error interface. Timeout()
	// alone doesn't cover DNS failures, so we also check the string.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return errNetwork
	}

	// Fallback string-match heuristics for wrapped errors that don't
	// surface via errors.As (some HTTP client wrappers eat types).
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "429") || strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "secondary rate limit") || strings.Contains(msg, "abuse"):
		return errRateLimit
	case strings.Contains(msg, "404") || strings.Contains(msg, "not found") ||
		strings.Contains(msg, "422") || strings.Contains(msg, "validation") ||
		strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "permission denied"):
		return errNonRetryable
	case strings.Contains(msg, "500") || strings.Contains(msg, "502") ||
		strings.Contains(msg, "503") || strings.Contains(msg, "504") ||
		strings.Contains(msg, "bad gateway") || strings.Contains(msg, "gateway timeout"):
		return errServerSide
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "connection refused") || strings.Contains(msg, "eof"):
		return errNetwork
	}

	// Unknown  -  treat as retryable but only within backoff/MaxAge. The
	// alternative (drop) is worse: the whole point of this queue is to
	// stop losing jobs to unexpected transient failures.
	return errUnknownRetryable
}

// providerName is a nil-safe accessor for logging.
func providerName(ev providers.JobEvent) string {
	if ev.Provider == nil {
		return ""
	}
	return ev.Provider.Name()
}
