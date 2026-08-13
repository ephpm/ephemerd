package github

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestRunnerBusy_RepoLevel pins the repo-scoped read of GitHub's own
// per-runner busy flag. It is the scheduler's fallback authority for the
// "never destroy a runner that is executing a job" invariant, used where
// ephemerd cannot introspect the runner's runtime locally.
func TestRunnerBusy_RepoLevel(t *testing.T) {
	tests := []struct {
		name string
		busy bool
	}{
		{name: "runner is running a job", busy: true},
		{name: "runner is idle", busy: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/testorg/repo1/actions/runners/42", func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewEncoder(w).Encode(map[string]any{
					"id":     42,
					"name":   "runner-42",
					"status": "online",
					"busy":   tt.busy,
				}); err != nil {
					t.Logf("encoding: %v", err)
				}
			})
			c, srv := newTestClientWithServer(t, mux)
			defer srv.Close()

			got, err := c.RunnerBusy(context.Background(), "repo1", 42)
			if err != nil {
				t.Fatalf("RunnerBusy: %v", err)
			}
			if got != tt.busy {
				t.Errorf("RunnerBusy = %v, want %v", got, tt.busy)
			}
		})
	}
}

// TestRunnerBusy_OrgLevel pins that an org-registered JIT runner is read
// through the org endpoint. A repo-scoped GET would 404 for it, and a 404
// reads as "not busy" — so getting the scope wrong would silently defeat
// the veto for every org-level pool.
func TestRunnerBusy_OrgLevel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/testorg/actions/runners/42", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{"id": 42, "busy": true}); err != nil {
			t.Logf("encoding: %v", err)
		}
	})
	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()
	c.cfg.Repos = nil // org-level

	got, err := c.RunnerBusy(context.Background(), "", 42)
	if err != nil {
		t.Fatalf("RunnerBusy: %v", err)
	}
	if !got {
		t.Error("RunnerBusy = false, want true")
	}
}

// TestRunnerBusy_GoneIsNotBusy pins the one error that is an answer: an
// ephemeral runner deregisters itself the moment its job finishes, so a
// 404 is the strongest possible evidence that nothing is running on it.
func TestRunnerBusy_GoneIsNotBusy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/repo1/actions/runners/42", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	got, err := c.RunnerBusy(context.Background(), "repo1", 42)
	if err != nil {
		t.Fatalf("RunnerBusy: %v", err)
	}
	if got {
		t.Error("a deregistered runner reported busy")
	}
}

// TestRunnerBusy_ErrorIsNotAnAnswer pins the fail-safe contract at the
// API boundary: anything other than a clean read or a 404 must surface as
// an error, so the scheduler treats the runner as possibly busy rather
// than as idle.
func TestRunnerBusy_ErrorIsNotAnAnswer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/repo1/actions/runners/42", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"rate limit exceeded"}`, http.StatusForbidden)
	})
	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	if _, err := c.RunnerBusy(context.Background(), "repo1", 42); err == nil {
		t.Fatal("want an error for a failed read; a rate limit must not read as idle")
	}
}
