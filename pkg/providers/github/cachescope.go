package github

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/ephpm/ephemerd/pkg/github"
	"github.com/ephpm/ephemerd/pkg/providers"
)

var _ providers.CacheScoper = (*Provider)(nil)

// CacheScope resolves the LAN cache scope for the job in event from GitHub's
// record of its run.
func (p *Provider) CacheScope(ctx context.Context, event *providers.JobEvent) (providers.CacheScope, error) {
	if event == nil || event.RunID == 0 || event.Repo == "" {
		return providers.CacheScope{}, errors.New("event carries no run to resolve a cache scope from")
	}
	f, err := p.client.RunCacheFacts(ctx, event.Repo, event.RunID)
	if err != nil {
		return providers.CacheScope{}, err
	}
	return cacheScopeFor(f), nil
}

// trustedPushEvents run code that is already on a branch of the repository,
// so a job may save under that branch — exactly as GitHub's own cache
// allows.
var trustedPushEvents = map[string]bool{
	"push":                true,
	"schedule":            true,
	"workflow_dispatch":   true,
	"repository_dispatch": true,
	"release":             true,
	"create":              true,
	"merge_group":         true,
}

// cacheScopeFor is the policy, kept pure so it can be tested in full.
//
//   - Branch events (push, schedule, dispatch, …) save under their own
//     branch and restore from it, then the default branch.
//   - pull_request saves under the PR's merge ref and restores from it, then
//     the default branch; nothing a PR saves is ever visible to a branch.
//     A fork PR is treated the same way — its writes are confined to its own
//     PR scope — and when GitHub does not name the PR (it omits fork PRs from
//     the run payload) the scope is the run itself, so it is shared with
//     nothing.
//   - Everything else (pull_request_target, issue_comment, workflow_run, …)
//     is READ-ONLY. GitHub would let some of these save under the default
//     branch, but they are the events that run on behalf of outsiders, and
//     a poisoned default-branch entry is restored by every job in the repo.
//     A miss costs minutes; a poisoned cache costs the build.
//   - A head repository that differs from the run's repository on any
//     event other than pull_request is read-only too: that combination does
//     not occur legitimately.
func cacheScopeFor(f github.RunCacheFacts) providers.CacheScope {
	s := providers.CacheScope{Repo: f.Repo}
	def := ""
	if f.DefaultBranch != "" {
		def = "refs/heads/" + f.DefaultBranch
	}
	fork := f.HeadRepo != "" && !strings.EqualFold(f.HeadRepo, f.Repo)

	switch {
	case f.Event == "pull_request":
		own := "run/" + strconv.FormatInt(f.RunID, 10)
		if f.PRNumber > 0 {
			own = "refs/pull/" + strconv.Itoa(f.PRNumber) + "/merge"
		}
		s.Write = own
		s.Reads = []string{own}
	case trustedPushEvents[f.Event] && !fork && f.HeadBranch != "":
		own := "refs/heads/" + f.HeadBranch
		s.Write = own
		s.Reads = []string{own}
	}
	if def != "" && (len(s.Reads) == 0 || s.Reads[0] != def) {
		s.Reads = append(s.Reads, def)
	}
	return s
}
