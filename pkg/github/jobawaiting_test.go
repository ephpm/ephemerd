package github

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestJobAwaitingRunner pins the status mapping the scheduler relies on to
// drop stale dispatches. Getting a live status wrong strands a real job (a
// "gone" answer is never second-guessed), so every status that can still end
// up needing a runner must read as awaiting.
func TestJobAwaitingRunner(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"queued", true},
		{"waiting", true},   // environment approval: will need a runner once approved
		{"pending", true},   // concurrency group: ditto
		{"requested", true}, // ditto
		{"in_progress", false},
		{"completed", false},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/testorg/repo1/actions/jobs/77", func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewEncoder(w).Encode(map[string]any{"id": 77, "status": tt.status}); err != nil {
					t.Logf("encoding: %v", err)
				}
			})
			c, srv := newTestClientWithServer(t, mux)
			defer srv.Close()

			got, err := c.JobAwaitingRunner(context.Background(), "repo1", 77)
			if err != nil {
				t.Fatalf("JobAwaitingRunner: %v", err)
			}
			if got != tt.want {
				t.Errorf("JobAwaitingRunner(status=%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

// A job whose run was deleted 404s. It cannot need a runner, and reporting it
// as an error would make the caller fail open and boot one for it anyway.
func TestJobAwaitingRunner_NotFoundIsGone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/repo1/actions/jobs/77", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	got, err := c.JobAwaitingRunner(context.Background(), "repo1", 77)
	if err != nil {
		t.Fatalf("JobAwaitingRunner on 404: %v, want nil error", err)
	}
	if got {
		t.Error("JobAwaitingRunner on 404 = true, want false")
	}
}

// Any other failure must surface as an error, so the scheduler can fail open
// rather than mistake an API outage for "the job is gone".
func TestJobAwaitingRunner_ServerErrorIsUnknown(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/repo1/actions/jobs/77", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	if _, err := c.JobAwaitingRunner(context.Background(), "repo1", 77); err == nil {
		t.Error("JobAwaitingRunner on 500 returned nil error; an outage would read as a definite answer")
	}
}
