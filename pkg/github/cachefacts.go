package github

import (
	"context"
	"fmt"
	"time"
)

// RunCacheFacts is the part of a workflow run that decides which LAN cache
// scope its jobs get. Everything here comes from GitHub's API, not from the
// job.
type RunCacheFacts struct {
	// Repo is the run's repository as "owner/repo".
	Repo string
	// Event is what triggered the run ("push", "pull_request", …).
	Event string
	// HeadBranch is the branch (or, for a tag push, the tag) the run is for.
	HeadBranch string
	// HeadRepo is the repository the head commit lives in, "owner/repo".
	// It differs from Repo for a pull request from a fork.
	HeadRepo string
	// PRNumber is the pull request the run belongs to, when GitHub lists
	// one (it does not for fork PRs). 0 otherwise.
	PRNumber int
	// DefaultBranch is the repository's default branch.
	DefaultBranch string
	RunID         int64
}

type defaultBranchEntry struct {
	branch  string
	fetched time.Time
}

const defaultBranchTTL = time.Hour

// RunCacheFacts fetches the facts for one run of repo (a name under this
// client's owner). It costs one API call per run, plus one per repository
// per hour for the default branch, which the run payload does not reliably
// carry.
func (c *Client) RunCacheFacts(ctx context.Context, repo string, runID int64) (RunCacheFacts, error) {
	run, _, err := c.client.Actions.GetWorkflowRunByID(ctx, c.cfg.Owner, repo, runID)
	if err != nil {
		return RunCacheFacts{}, fmt.Errorf("fetching run %d of %s/%s: %w", runID, c.cfg.Owner, repo, err)
	}
	f := RunCacheFacts{
		Repo:       run.GetRepository().GetFullName(),
		Event:      run.GetEvent(),
		HeadBranch: run.GetHeadBranch(),
		HeadRepo:   run.GetHeadRepository().GetFullName(),
		RunID:      runID,
	}
	if f.Repo == "" {
		f.Repo = c.cfg.Owner + "/" + repo
	}
	for _, pr := range run.PullRequests {
		if n := pr.GetNumber(); n > 0 {
			f.PRNumber = n
			break
		}
	}
	f.DefaultBranch = run.GetRepository().GetDefaultBranch()
	if f.DefaultBranch == "" {
		if v, ok := c.defaultBranches.Load(repo); ok {
			if e := v.(defaultBranchEntry); time.Since(e.fetched) < defaultBranchTTL {
				f.DefaultBranch = e.branch
			}
		}
	}
	if f.DefaultBranch == "" {
		r, _, err := c.client.Repositories.Get(ctx, c.cfg.Owner, repo)
		if err != nil {
			return RunCacheFacts{}, fmt.Errorf("fetching default branch of %s/%s: %w", c.cfg.Owner, repo, err)
		}
		f.DefaultBranch = r.GetDefaultBranch()
		c.defaultBranches.Store(repo, defaultBranchEntry{branch: f.DefaultBranch, fetched: time.Now()})
	}
	return f, nil
}
