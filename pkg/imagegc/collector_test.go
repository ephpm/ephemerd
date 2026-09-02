package imagegc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testCollector builds a Collector with the four containerd-facing calls
// stubbed. New() cannot be used here — it requires a live *containerd.Client
// — which is precisely why CollectAll had no coverage at all despite being
// the operator's disk-recovery command.
//
// Thresholds default to "always over" (an impossible MinFreeBytes) so the
// watermark pass has something to do without needing a full disk; individual
// tests override.
func testCollector(t *testing.T, cands []Candidate) *Collector {
	t.Helper()
	c := &Collector{
		cfg: Config{
			Path:             t.TempDir(),
			Thresholds:       Thresholds{MinFreeBytes: math.MaxInt64},
			ExhaustedBackoff: DefaultExhaustedBackoff,
			Log:              quietLog(),
		},
		now: time.Now,
		resolveNamespacesFn: func(context.Context, *client.Client, []string, []string) ([]string, error) {
			return []string{"ephemerd"}, nil
		},
		runningContainersFn: func(context.Context, *client.Client, *slog.Logger) (map[string]struct{}, map[string]struct{}, error) {
			return map[string]struct{}{}, map[string]struct{}{}, nil
		},
		listCandidatesFn: func(context.Context, *client.Client, []string, *slog.Logger) ([]Candidate, error) {
			return cands, nil
		},
	}
	return c
}

func candidate(ns, name string, size int64, age time.Duration) Candidate {
	return Candidate{
		Namespace:    ns,
		Name:         name,
		SizeBytes:    size,
		LastAccessed: time.Now().UTC().Add(-age),
	}
}

// TestCollectAll_ForcesThePlannerOpen pins the CollectAll -> collect(force)
// -> PlanForced wiring.
//
// PlanEviction is watermark-driven: on a node whose thresholds are NOT
// tripped it correctly plans nothing. That is right for the timer and wrong
// for `ephemerd cache clear containerd --all`, and getting it wrong is the
// bug this whole path exists to fix — the operator saw "0 records removed"
// and concluded the command was broken. So the assertion is specifically
// that a forced pass evicts everything unprotected on a disk that is under
// no pressure at all.
func TestCollectAll_ForcesThePlannerOpen(t *testing.T) {
	cands := []Candidate{
		candidate("ephemerd", "docker.io/library/alpine:3", 100, time.Hour),
		candidate("ephemerd", "docker.io/library/golang:1", 200, 2*time.Hour),
		candidate("ephemerd", "ghcr.io/pinned/runner:v1", 300, 3*time.Hour),
	}
	c := testCollector(t, cands)
	// No pressure whatsoever: a plain Collect must plan nothing.
	c.cfg.Thresholds = Thresholds{HighUsedPercent: 100.1} // unreachable: UsedPercent() never exceeds 100, so "not over" is deterministic
	c.cfg.PinnedImages = []string{"ghcr.io/pinned/runner:v1"}

	var evicted []Candidate
	c.evictFn = func(_ context.Context, _ *client.Client, cs []Candidate, _ bool, _ *slog.Logger, _ func() bool) int {
		evicted = append(evicted, cs...)
		return len(cs)
	}

	res, err := c.CollectAll(context.Background())
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if !res.Forced {
		t.Error("Result.Forced = false on a CollectAll pass")
	}
	if res.Evicted != 2 {
		t.Errorf("evicted %d records, want 2 — a forced pass must ignore the watermarks, which is the entire point of --all", res.Evicted)
	}
	if res.Protected != 1 {
		t.Errorf("protected = %d, want 1 — --all forces the POLICY open, never the safety", res.Protected)
	}
	for _, e := range evicted {
		if e.Name == "ghcr.io/pinned/runner:v1" {
			t.Fatal("a pinned runner image was evicted by --all; the protected set is an absolute veto")
		}
	}

	// Control: the same collector under the same (absent) pressure must
	// plan nothing on the automatic path. Without this the assertion above
	// would also pass if force were being ignored and the disk happened to
	// look full.
	c2 := testCollector(t, cands)
	c2.cfg.Thresholds = Thresholds{HighUsedPercent: 100.1} // unreachable: UsedPercent() never exceeds 100, so "not over" is deterministic
	c2.evictFn = func(context.Context, *client.Client, []Candidate, bool, *slog.Logger, func() bool) int {
		t.Fatal("the watermark pass evicted something on an unpressured disk")
		return 0
	}
	if _, err := c2.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
}

// TestCollectAll_AbortsWhenTheLiveSetIsUnknown: without the running-container
// set there is no way to guarantee we do not evict an image a job in flight
// needs. Forcing the policy open must not force this safety open too — the
// pass has to abort before anything is deleted.
func TestCollectAll_AbortsWhenTheLiveSetIsUnknown(t *testing.T) {
	boom := errors.New("containerd: connection refused")
	c := testCollector(t, []Candidate{candidate("ephemerd", "alpine:3", 100, time.Hour)})
	c.runningContainersFn = func(context.Context, *client.Client, *slog.Logger) (map[string]struct{}, map[string]struct{}, error) {
		return nil, nil, boom
	}
	c.evictFn = func(context.Context, *client.Client, []Candidate, bool, *slog.Logger, func() bool) int {
		t.Fatal("records were evicted after the live-container lookup failed; a running job's image could have gone with them")
		return 0
	}

	_, err := c.CollectAll(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("CollectAll err = %v, want the containerd error so the operator sees why nothing happened", err)
	}
}

// TestCollectAll_DoesNotArmTheExhaustedBackoff: the failsafe exists to stop
// the DAEMON'S OWN TIMER spinning on a store it cannot shrink. A human typing
// --all is not the daemon's timer, and arming it here would silence the
// automatic collector for 30 minutes on a node that is genuinely filling up.
func TestCollectAll_DoesNotArmTheExhaustedBackoff(t *testing.T) {
	c := testCollector(t, nil) // nothing evictable, and always "over"
	c.evictFn = func(context.Context, *client.Client, []Candidate, bool, *slog.Logger, func() bool) int { return 0 }

	res, err := c.CollectAll(context.Background())
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if res.Exhausted {
		t.Error("Result.Exhausted set by a forced pass; it was never chasing a watermark")
	}
	if !c.exhaustedUntil.IsZero() {
		t.Fatalf("forced pass armed the backoff until %v — one operator `cache clear --all` would mute the automatic collector", c.exhaustedUntil)
	}
}

// TestCollectAll_ClearsAnArmedExhaustedBackoff is the mirror image, and the
// half that was missing.
//
// The backoff's premise is "everything evictable is gone and we are still
// over the line". An operator running --all evicts a SUPERSET of what the
// automatic pass is allowed to touch, so a forced pass that actually frees
// records has falsified that premise. Leaving the suppression armed meant
// --all freed the disk and the automatic collector stayed muted for up to 30
// more minutes anyway — on a false premise, right after a human fixed the
// node.
func TestCollectAll_ClearsAnArmedExhaustedBackoff(t *testing.T) {
	c := testCollector(t, []Candidate{candidate("ephemerd", "alpine:3", 100, time.Hour)})
	armedUntil := time.Now().Add(29 * time.Minute)
	c.exhaustedUntil = armedUntil
	c.evictFn = func(_ context.Context, _ *client.Client, cs []Candidate, _ bool, _ *slog.Logger, _ func() bool) int {
		return len(cs)
	}

	if _, err := c.CollectAll(context.Background()); err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if !c.exhaustedUntil.IsZero() {
		t.Fatalf("backoff still armed until %v after a forced pass reclaimed records; the automatic collector stays suppressed on a premise the operator just falsified", c.exhaustedUntil)
	}

	// A forced pass that reclaimed NOTHING has falsified nothing, so it must
	// leave an armed backoff alone (and still must not arm one).
	c2 := testCollector(t, nil)
	c2.exhaustedUntil = armedUntil
	c2.evictFn = func(context.Context, *client.Client, []Candidate, bool, *slog.Logger, func() bool) int { return 0 }
	if _, err := c2.CollectAll(context.Background()); err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if !c2.exhaustedUntil.Equal(armedUntil) {
		t.Errorf("exhaustedUntil = %v, want it untouched at %v — a pass that freed nothing disproves nothing", c2.exhaustedUntil, armedUntil)
	}
}

// TestCollectAll_IgnoresTheBackoffSuppression: the automatic pass skips while
// the failsafe is armed. An operator override must run anyway, or `--all` is
// unusable for up to 30 minutes after exactly the event that makes someone
// want to run it.
func TestCollectAll_IgnoresTheBackoffSuppression(t *testing.T) {
	c := testCollector(t, []Candidate{candidate("ephemerd", "alpine:3", 100, time.Hour)})
	c.exhaustedUntil = time.Now().Add(29 * time.Minute)
	c.evictFn = func(_ context.Context, _ *client.Client, cs []Candidate, _ bool, _ *slog.Logger, _ func() bool) int {
		return len(cs)
	}

	res, err := c.CollectAll(context.Background())
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if res.Skipped {
		t.Fatal("forced pass was skipped by the exhausted backoff; --all must not be gated on the daemon's own failsafe")
	}
	if res.Evicted != 1 {
		t.Errorf("evicted = %d, want 1", res.Evicted)
	}

	// The automatic pass, by contrast, must still be suppressed.
	c2 := testCollector(t, []Candidate{candidate("ephemerd", "alpine:3", 100, time.Hour)})
	c2.exhaustedUntil = time.Now().Add(29 * time.Minute)
	res2, err := c2.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !res2.Skipped {
		t.Error("automatic pass was not suppressed by an armed backoff")
	}
}

// TestCollectAll_NoStopFunction: a forced pass has no stopping point short of
// the whole plan. The watermark pass passes a stop closure that halts the
// moment a real reading clears; passing one on a forced pass would make --all
// stop early and leave the operator with a partial clear they did not ask
// for.
func TestCollectAll_NoStopFunction(t *testing.T) {
	c := testCollector(t, []Candidate{candidate("ephemerd", "alpine:3", 100, time.Hour)})
	var gotStop bool
	c.evictFn = func(_ context.Context, _ *client.Client, cs []Candidate, _ bool, _ *slog.Logger, stop func() bool) int {
		gotStop = stop != nil
		return len(cs)
	}
	if _, err := c.CollectAll(context.Background()); err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if gotStop {
		t.Error("a forced pass supplied a stop function; --all must run the whole plan")
	}
}

// TestNilCollectorCollectAll: callers invoke the collector unconditionally,
// and a node without image GC configured has a nil one.
func TestNilCollectorCollectAll(t *testing.T) {
	var c *Collector
	res, err := c.CollectAll(context.Background())
	if err != nil {
		t.Fatalf("nil CollectAll: %v", err)
	}
	if res.Evicted != 0 {
		t.Errorf("nil collector evicted %d", res.Evicted)
	}
}
