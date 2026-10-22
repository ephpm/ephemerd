package github

import (
	"strings"
	"testing"

	"github.com/ephpm/ephemerd/pkg/github"
)

func TestCacheScopeFor(t *testing.T) {
	base := github.RunCacheFacts{Repo: "ephpm/ephpm", HeadRepo: "ephpm/ephpm", DefaultBranch: "main", RunID: 42}
	with := func(mut func(*github.RunCacheFacts)) github.RunCacheFacts {
		f := base
		mut(&f)
		return f
	}
	for _, tc := range []struct {
		name  string
		f     github.RunCacheFacts
		write string
		reads string
	}{
		{"push to main", with(func(f *github.RunCacheFacts) { f.Event, f.HeadBranch = "push", "main" }),
			"refs/heads/main", "refs/heads/main"},
		{"push to a feature branch", with(func(f *github.RunCacheFacts) { f.Event, f.HeadBranch = "push", "feat/x" }),
			"refs/heads/feat/x", "refs/heads/feat/x,refs/heads/main"},
		{"schedule", with(func(f *github.RunCacheFacts) { f.Event, f.HeadBranch = "schedule", "main" }),
			"refs/heads/main", "refs/heads/main"},
		{"same-repo PR", with(func(f *github.RunCacheFacts) { f.Event, f.HeadBranch, f.PRNumber = "pull_request", "feat/x", 7 }),
			"refs/pull/7/merge", "refs/pull/7/merge,refs/heads/main"},
		// The poisoning case: a fork names its branch "main". It must land in
		// its own PR/run scope, never in refs/heads/main.
		{"fork PR with a branch called main", with(func(f *github.RunCacheFacts) {
			f.Event, f.HeadBranch, f.HeadRepo = "pull_request", "main", "attacker/ephpm"
		}), "run/42", "run/42,refs/heads/main"},
		{"pull_request_target is read-only", with(func(f *github.RunCacheFacts) { f.Event, f.HeadBranch = "pull_request_target", "main" }),
			"", "refs/heads/main"},
		{"issue_comment is read-only", with(func(f *github.RunCacheFacts) { f.Event, f.HeadBranch = "issue_comment", "main" }),
			"", "refs/heads/main"},
		{"push from a foreign head repo is read-only", with(func(f *github.RunCacheFacts) {
			f.Event, f.HeadBranch, f.HeadRepo = "push", "main", "attacker/ephpm"
		}), "", "refs/heads/main"},
		{"unknown default branch", with(func(f *github.RunCacheFacts) { f.Event, f.HeadBranch, f.DefaultBranch = "push", "dev", "" }),
			"refs/heads/dev", "refs/heads/dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := cacheScopeFor(tc.f)
			if s.Repo != "ephpm/ephpm" {
				t.Errorf("Repo = %q", s.Repo)
			}
			if s.Write != tc.write {
				t.Errorf("Write = %q, want %q", s.Write, tc.write)
			}
			if got := strings.Join(s.Reads, ","); got != tc.reads {
				t.Errorf("Reads = %q, want %q", got, tc.reads)
			}
			if s.Write != "" && s.Write == "refs/heads/main" && tc.f.Event != "push" && tc.f.Event != "schedule" {
				t.Errorf("event %q got write access to the default branch", tc.f.Event)
			}
		})
	}
}
