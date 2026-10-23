package scheduler

import (
	"context"
	goruntime "runtime"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// ActionsCache is the scheduler's view of the LAN Actions cache
// (pkg/proxies/actionscache). The lifecycle it drives:
//
//  1. Create: the runner is Tracked and its container gets ResultsURL. It is
//     UNBOUND — every lookup misses, every save is refused.
//  2. in_progress: GitHub names the job that actually took the runner. Its
//     scope is resolved from the provider's record of that job's run and
//     Bound. Binding on the dispatched job instead would be wrong: same-label
//     JIT runners are fungible (and org-level), so a runner started for a
//     push to main can be handed a fork PR.
//  3. Teardown: the runner is Forgotten, revoking everything it was granted.
type ActionsCache interface {
	Healthy() bool
	ResultsURL() string
	Track(runner string)
	Bind(runner string, s providers.CacheScope)
	Forget(runner string)
}

// actionsCacheBindTimeout bounds the provider lookup behind a binding. A
// slow lookup only delays the job's first cache hit; it never blocks the
// event loop.
const actionsCacheBindTimeout = 30 * time.Second

// actionsCacheFor decides whether a job about to be created gets the LAN
// cache, and if so tracks its runner and returns the ACTIONS_RESULTS_URL to
// give it. "" means the job runs exactly as it would without the feature.
func (s *Scheduler) actionsCacheFor(event providers.JobEvent, runner string) string {
	ac := s.cfg.ActionsCache
	if ac == nil || goruntime.GOOS != "linux" || !ac.Healthy() {
		return ""
	}
	if _, ok := event.Provider.(providers.CacheScoper); !ok {
		return ""
	}
	ac.Track(runner)
	return ac.ResultsURL()
}

// forgetActionsCache revokes a runner's cache access. Nil-safe.
func (s *Scheduler) forgetActionsCache(runner string) {
	if ac := s.cfg.ActionsCache; ac != nil && runner != "" {
		ac.Forget(runner)
	}
}

// bindActionsCache resolves and binds the cache scope of the job that took
// runner. Runs in its own goroutine from handleInProgress. Any failure
// leaves the runner unbound — cache misses, never a wider scope.
func (s *Scheduler) bindActionsCache(event providers.JobEvent, runner string) {
	ac := s.cfg.ActionsCache
	if ac == nil {
		return
	}
	sc, ok := event.Provider.(providers.CacheScoper)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), actionsCacheBindTimeout)
	defer cancel()
	scope, err := sc.CacheScope(ctx, &event)
	if err != nil {
		s.cfg.Log.Warn("could not resolve the job's Actions cache scope; it runs without the LAN cache",
			"runner", runner, "job_id", event.JobID, "repo", event.Repo, "error", err)
		return
	}
	ac.Bind(runner, scope)
}
