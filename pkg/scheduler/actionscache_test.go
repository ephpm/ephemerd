package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

type fakeActionsCache struct {
	healthy bool
	mu      sync.Mutex
	tracked map[string]bool
	bound   map[string]providers.CacheScope
}

func newFakeActionsCache() *fakeActionsCache {
	return &fakeActionsCache{healthy: true, tracked: map[string]bool{}, bound: map[string]providers.CacheScope{}}
}

func (f *fakeActionsCache) Healthy() bool      { return f.healthy }
func (f *fakeActionsCache) ResultsURL() string { return "http://10.88.0.1:8089/" }
func (f *fakeActionsCache) Track(r string)     { f.mu.Lock(); f.tracked[r] = true; f.mu.Unlock() }
func (f *fakeActionsCache) Forget(r string) {
	f.mu.Lock()
	delete(f.tracked, r)
	delete(f.bound, r)
	f.mu.Unlock()
}
func (f *fakeActionsCache) Bind(r string, s providers.CacheScope) {
	f.mu.Lock()
	f.bound[r] = s
	f.mu.Unlock()
}

// scopingProvider adds CacheScoper to the poll mock, answering from the
// event's run id so a test can tell WHICH job a binding came from.
type scopingProvider struct {
	*mockProvider
	err error
}

func (p *scopingProvider) CacheScope(_ context.Context, ev *providers.JobEvent) (providers.CacheScope, error) {
	if p.err != nil {
		return providers.CacheScope{}, p.err
	}
	if ev.RunID == 1 {
		return providers.CacheScope{Repo: "o/r", Write: "refs/heads/main", Reads: []string{"refs/heads/main"}}, nil
	}
	return providers.CacheScope{Repo: "o/r", Write: "refs/pull/9/merge", Reads: []string{"refs/pull/9/merge", "refs/heads/main"}}, nil
}

func newCacheTestScheduler(ac ActionsCache) *Scheduler {
	return &Scheduler{cfg: Config{ActionsCache: ac, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
}

func TestActionsCacheFor(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("the LAN Actions cache is Linux-only")
	}
	ac := newFakeActionsCache()
	s := newCacheTestScheduler(ac)
	scoper := &scopingProvider{mockProvider: newMockProvider("github")}

	if url := s.actionsCacheFor(providers.JobEvent{Provider: scoper}, "r1"); url == "" || !ac.tracked["r1"] {
		t.Fatalf("healthy cache + scoping provider: url %q tracked %v", url, ac.tracked["r1"])
	}
	if url := s.actionsCacheFor(providers.JobEvent{Provider: newMockProvider("forgejo")}, "r2"); url != "" || ac.tracked["r2"] {
		t.Fatal("a provider that cannot resolve scopes got the cache")
	}
	ac.healthy = false
	if url := s.actionsCacheFor(providers.JobEvent{Provider: scoper}, "r3"); url != "" || ac.tracked["r3"] {
		t.Fatal("an unhealthy cache was turned on for a new job")
	}
	if url := newCacheTestScheduler(nil).actionsCacheFor(providers.JobEvent{Provider: scoper}, "r4"); url != "" {
		t.Fatal("cache disabled but a URL was handed out")
	}
}

// The binding follows the job GitHub reports in in_progress — here a PR
// (run 2) that took a runner dispatched for a push to main (run 1). It must
// get the PR's scope, not main's.
func TestBindActionsCache_FollowsTheObservedJob(t *testing.T) {
	ac := newFakeActionsCache()
	s := newCacheTestScheduler(ac)
	scoper := &scopingProvider{mockProvider: newMockProvider("github")}

	s.bindActionsCache(providers.JobEvent{Provider: scoper, RunID: 2, Repo: "r"}, "dispatched-for-main")
	got := ac.bound["dispatched-for-main"]
	if got.Write != "refs/pull/9/merge" {
		t.Fatalf("runner bound with write %q; the PR that actually took it must not inherit main's scope", got.Write)
	}
}

func TestBindActionsCache_LookupFailureLeavesUnbound(t *testing.T) {
	ac := newFakeActionsCache()
	s := newCacheTestScheduler(ac)
	scoper := &scopingProvider{mockProvider: newMockProvider("github"), err: errors.New("api down")}
	s.bindActionsCache(providers.JobEvent{Provider: scoper, RunID: 1, Repo: "r"}, "r1")
	if _, ok := ac.bound["r1"]; ok {
		t.Fatal("a failed scope lookup still bound a scope")
	}
}

// countingScoper counts scope lookups, each of which is a GitHub API call.
type countingScoper struct {
	*mockProvider
	mu sync.Mutex
	n  int
}

func (p *countingScoper) CacheScope(context.Context, *providers.JobEvent) (providers.CacheScope, error) {
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
	return providers.CacheScope{Repo: "o/r", Write: "refs/heads/main", Reads: []string{"refs/heads/main"}}, nil
}

// Polling reports a running job on every poll, not once like a webhook.
// The binding — one API lookup — must happen on the first report only.
func TestHandleInProgress_RepeatedReportsBindOnce(t *testing.T) {
	ac := newFakeActionsCache()
	prov := &countingScoper{mockProvider: newMockProvider("github")}
	s := New(Config{Providers: []providers.Provider{prov}, ActionsCache: ac, Log: quietLogger()})
	key := jobKey{Provider: prov.Name(), JobID: 7}
	s.mu.Lock()
	s.runners["r1"] = &runnerBinding{intentKey: key}
	s.mu.Unlock()

	ev := providers.JobEvent{Provider: prov, Action: "in_progress", Repo: "r", JobID: 7, RunnerName: "r1"}
	for i := 0; i < 5; i++ {
		s.handleInProgress(ev)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		ac.mu.Lock()
		_, bound := ac.bound["r1"]
		ac.mu.Unlock()
		if bound || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let any duplicate binding goroutine run
	prov.mu.Lock()
	n := prov.n
	prov.mu.Unlock()
	if n != 1 {
		t.Fatalf("scope looked up %d times for one job; want 1", n)
	}
}
