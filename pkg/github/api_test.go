package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// newTestClientWithServer creates a Client backed by a mock HTTP server.
// The mock URL is automatically set as the go-github client's BaseURL.
func newTestClientWithServer(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)

	ghClient := gh.NewClient(nil).WithAuthToken("test-token")
	u, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	ghClient.BaseURL = u

	c := &Client{
		cfg: Config{
			Token: "test-token",
			Owner: "testorg",
			Repos: []string{"repo1"},
			Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		client: ghClient,
	}
	return c, srv
}

// --- PollJobs tests ---

func TestPollJobs_FindsQueuedSelfHostedJobs(t *testing.T) {
	mux := http.NewServeMux()

	// Mock: list workflow runs for repo1 (queued)
	mux.HandleFunc("/repos/testorg/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "queued" {
			w.WriteHeader(200)
			if err := json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []any{}}); err != nil {
				t.Logf("encoding: %v", err)
			}
			return
		}
		resp := map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 100, "status": "queued"},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	// Mock: list jobs for run 100
	mux.HandleFunc("/repos/testorg/repo1/actions/runs/100/jobs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"jobs": []map[string]any{
				{
					"id":     200,
					"status": "queued",
					"labels": []string{"self-hosted", "linux", "x64"},
				},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	events, err := c.PollJobs(context.Background())
	if err != nil {
		t.Fatalf("PollJobs() error: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("PollJobs() returned %d events, want 1", len(events))
	}
	if events[0].Action != "queued" {
		t.Errorf("event action = %q, want %q", events[0].Action, "queued")
	}
	if events[0].Repo != "repo1" {
		t.Errorf("event repo = %q, want %q", events[0].Repo, "repo1")
	}
	if events[0].Job.GetID() != 200 {
		t.Errorf("event job ID = %d, want 200", events[0].Job.GetID())
	}
}

func TestPollJobs_SkipsNonSelfHosted(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/testorg/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 100, "status": "queued"},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	mux.HandleFunc("/repos/testorg/repo1/actions/runs/100/jobs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"jobs": []map[string]any{
				{
					"id":     300,
					"status": "queued",
					"labels": []string{"ubuntu-latest"}, // NOT self-hosted
				},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	events, err := c.PollJobs(context.Background())
	if err != nil {
		t.Fatalf("PollJobs() error: %v", err)
	}

	if len(events) != 0 {
		t.Errorf("PollJobs() returned %d events, want 0 (non-self-hosted)", len(events))
	}
}

func TestPollJobs_NoQueuedRuns(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/testorg/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"total_count": 0, "workflow_runs": []any{}}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	events, err := c.PollJobs(context.Background())
	if err != nil {
		t.Fatalf("PollJobs() error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("PollJobs() returned %d events, want 0", len(events))
	}
}

// --- RegisterWebhooks tests ---

func TestRegisterWebhooks_RepoLevel(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/testorg/repo1/hooks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding body: %v", err)
		}

		// Verify the webhook config
		config, ok := body["config"].(map[string]any)
		if !ok {
			t.Fatal("missing config in request body")
		}
		if config["url"] != "https://tunnel.example.com/webhook" {
			t.Errorf("url = %v", config["url"])
		}

		w.WriteHeader(201)
		resp := map[string]any{"id": 999}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	hooks, err := c.RegisterWebhooks(context.Background(), "https://tunnel.example.com/webhook", "secret123")
	if err != nil {
		t.Fatalf("RegisterWebhooks() error: %v", err)
	}

	if len(hooks) != 1 {
		t.Fatalf("got %d hooks, want 1", len(hooks))
	}
	if hooks[0].Repo != "repo1" {
		t.Errorf("hook repo = %q, want %q", hooks[0].Repo, "repo1")
	}
	if hooks[0].HookID != 999 {
		t.Errorf("hook ID = %d, want 999", hooks[0].HookID)
	}
}

func TestRegisterWebhooks_OrgLevel(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/orgs/testorg/hooks", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		resp := map[string]any{"id": 888}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	// Set repos to empty for org-level
	c.cfg.Repos = nil

	hooks, err := c.RegisterWebhooks(context.Background(), "https://tunnel.example.com/webhook", "secret")
	if err != nil {
		t.Fatalf("RegisterWebhooks() error: %v", err)
	}

	if len(hooks) != 1 {
		t.Fatalf("got %d hooks, want 1", len(hooks))
	}
	if hooks[0].Repo != "" {
		t.Errorf("org-level hook should have empty repo, got %q", hooks[0].Repo)
	}
	if hooks[0].HookID != 888 {
		t.Errorf("hook ID = %d, want 888", hooks[0].HookID)
	}
}

func TestRegisterWebhooks_APIError(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/testorg/repo1/hooks", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		if _, err := w.Write([]byte(`{"message":"Validation Failed"}`)); err != nil {
			t.Logf("writing: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	_, err := c.RegisterWebhooks(context.Background(), "https://tunnel.example.com/webhook", "secret")
	if err == nil {
		t.Fatal("expected error for 422 response")
	}
}

// --- DeregisterWebhooks tests ---

func TestDeregisterWebhooks_RepoLevel(t *testing.T) {
	var deleted bool
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/testorg/repo1/hooks/999", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		deleted = true
		w.WriteHeader(204)
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	hooks := []ManagedWebhook{{Repo: "repo1", HookID: 999}}
	c.DeregisterWebhooks(context.Background(), hooks)

	if !deleted {
		t.Error("webhook should have been deleted")
	}
}

func TestDeregisterWebhooks_OrgLevel(t *testing.T) {
	var deleted bool
	mux := http.NewServeMux()

	mux.HandleFunc("/orgs/testorg/hooks/888", func(w http.ResponseWriter, r *http.Request) {
		deleted = true
		w.WriteHeader(204)
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()
	c.cfg.Repos = nil

	hooks := []ManagedWebhook{{HookID: 888}}
	c.DeregisterWebhooks(context.Background(), hooks)

	if !deleted {
		t.Error("org webhook should have been deleted")
	}
}

// --- PollJobs org-level tests ---

func TestPollJobs_OrgLevel_DiscoverReposAndJobs(t *testing.T) {
	mux := http.NewServeMux()

	// Mock: list org repos
	mux.HandleFunc("/orgs/testorg/repos", func(w http.ResponseWriter, r *http.Request) {
		repos := []map[string]any{
			{"id": 1, "name": "repo-alpha", "full_name": "testorg/repo-alpha"},
			{"id": 2, "name": "repo-beta", "full_name": "testorg/repo-beta"},
		}
		if err := json.NewEncoder(w).Encode(repos); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	// Mock: queued run in repo-alpha
	mux.HandleFunc("/repos/testorg/repo-alpha/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 500, "status": "queued"},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	// Mock: jobs for run 500
	mux.HandleFunc("/repos/testorg/repo-alpha/actions/runs/500/jobs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"jobs": []map[string]any{
				{"id": 600, "status": "queued", "labels": []string{"self-hosted", "linux"}},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	// Mock: no queued runs in repo-beta
	mux.HandleFunc("/repos/testorg/repo-beta/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"total_count": 0, "workflow_runs": []any{}}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	// Set repos to nil for org-level polling
	c.cfg.Repos = nil

	events, err := c.PollJobs(context.Background())
	if err != nil {
		t.Fatalf("PollJobs() error: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("PollJobs() returned %d events, want 1", len(events))
	}
	if events[0].Repo != "repo-alpha" {
		t.Errorf("event repo = %q, want %q", events[0].Repo, "repo-alpha")
	}
	if events[0].Job.GetID() != 600 {
		t.Errorf("event job ID = %d, want 600", events[0].Job.GetID())
	}
}

func TestPollJobs_OrgLevel_ListReposError(t *testing.T) {
	mux := http.NewServeMux()

	// Mock: org repos endpoint returns 500
	mux.HandleFunc("/orgs/testorg/repos", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		if _, err := w.Write([]byte(`{"message":"Internal Server Error"}`)); err != nil {
			t.Logf("writing: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	c.cfg.Repos = nil

	_, err := c.PollJobs(context.Background())
	if err == nil {
		t.Fatal("expected error when ListByOrg returns 500")
	}
}

// --- pollRepo with multiple queued jobs across multiple runs ---

func TestPollJobs_MultipleRunsMultipleJobs(t *testing.T) {
	mux := http.NewServeMux()

	// Mock: two queued runs
	mux.HandleFunc("/repos/testorg/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 2,
			"workflow_runs": []map[string]any{
				{"id": 100, "status": "queued"},
				{"id": 200, "status": "queued"},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	// Mock: two jobs in run 100
	mux.HandleFunc("/repos/testorg/repo1/actions/runs/100/jobs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 2,
			"jobs": []map[string]any{
				{"id": 1001, "status": "queued", "labels": []string{"self-hosted", "linux"}},
				{"id": 1002, "status": "queued", "labels": []string{"self-hosted", "windows"}},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	// Mock: one job in run 200
	mux.HandleFunc("/repos/testorg/repo1/actions/runs/200/jobs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"jobs": []map[string]any{
				{"id": 2001, "status": "queued", "labels": []string{"self-hosted", "x64"}},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	events, err := c.PollJobs(context.Background())
	if err != nil {
		t.Fatalf("PollJobs() error: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("PollJobs() returned %d events, want 3", len(events))
	}

	// Collect job IDs
	ids := make(map[int64]bool)
	for _, e := range events {
		ids[e.Job.GetID()] = true
	}
	for _, wantID := range []int64{1001, 1002, 2001} {
		if !ids[wantID] {
			t.Errorf("expected job ID %d in events", wantID)
		}
	}
}

// --- PollJobs API error handling ---

func TestPollJobs_RepoRunsEndpointReturns500(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/testorg/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		if _, err := w.Write([]byte(`{"message":"Internal Server Error"}`)); err != nil {
			t.Logf("writing: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	// PollJobs with repo-level polling logs the error and continues (returns no events, no error)
	events, err := c.PollJobs(context.Background())
	if err != nil {
		t.Fatalf("PollJobs() should not return error for single repo failure, got: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("PollJobs() returned %d events, want 0", len(events))
	}
}

func TestPollJobs_ContextCancelled(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/testorg/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"total_count": 0, "workflow_runs": []any{}}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := c.PollJobs(ctx)
	// With a cancelled context, the HTTP request may fail
	// The function should either return an error or empty results
	if err != nil {
		t.Logf("PollJobs with cancelled context returned expected error: %v", err)
	}
}

// --- PollJobs partial failures (multi-repo) ---

func TestPollJobs_PartialFailure_OneRepoSucceedsOtherFails(t *testing.T) {
	mux := http.NewServeMux()

	// repo1 returns 500
	mux.HandleFunc("/repos/testorg/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		if _, err := w.Write([]byte(`{"message":"Internal Server Error"}`)); err != nil {
			t.Logf("writing: %v", err)
		}
	})

	// repo2 returns a valid queued job
	mux.HandleFunc("/repos/testorg/repo2/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 300, "status": "queued"},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	mux.HandleFunc("/repos/testorg/repo2/actions/runs/300/jobs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"jobs": []map[string]any{
				{"id": 400, "status": "queued", "labels": []string{"self-hosted", "linux"}},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	// Configure two repos
	c.cfg.Repos = []string{"repo1", "repo2"}

	events, err := c.PollJobs(context.Background())
	if err != nil {
		t.Fatalf("PollJobs() should not return error on partial failure, got: %v", err)
	}

	// Should still get the job from repo2 even though repo1 failed
	if len(events) != 1 {
		t.Fatalf("PollJobs() returned %d events, want 1 (from repo2)", len(events))
	}
	if events[0].Repo != "repo2" {
		t.Errorf("event repo = %q, want %q", events[0].Repo, "repo2")
	}
	if events[0].Job.GetID() != 400 {
		t.Errorf("event job ID = %d, want 400", events[0].Job.GetID())
	}
}

// --- Non-self-hosted filtering edge cases ---

func TestPollJobs_SkipsJobsWithEmptyLabels(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/testorg/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 100, "status": "queued"},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	mux.HandleFunc("/repos/testorg/repo1/actions/runs/100/jobs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"jobs": []map[string]any{
				{"id": 500, "status": "queued", "labels": []string{}},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	events, err := c.PollJobs(context.Background())
	if err != nil {
		t.Fatalf("PollJobs() error: %v", err)
	}

	if len(events) != 0 {
		t.Errorf("PollJobs() returned %d events, want 0 (empty labels)", len(events))
	}
}

func TestPollJobs_SkipsMixedLabelsWithoutSelfHosted(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/testorg/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 100, "status": "queued"},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	mux.HandleFunc("/repos/testorg/repo1/actions/runs/100/jobs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 2,
			"jobs": []map[string]any{
				{
					"id":     501,
					"status": "queued",
					"labels": []string{"ubuntu-latest", "linux", "x64"}, // no self-hosted
				},
				{
					"id":     502,
					"status": "queued",
					"labels": []string{"self-hosted", "linux", "x64"}, // has self-hosted
				},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	events, err := c.PollJobs(context.Background())
	if err != nil {
		t.Fatalf("PollJobs() error: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("PollJobs() returned %d events, want 1", len(events))
	}
	if events[0].Job.GetID() != 502 {
		t.Errorf("event job ID = %d, want 502 (only self-hosted job)", events[0].Job.GetID())
	}
}

func TestPollJobs_SkipsInProgressJobs(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/testorg/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 100, "status": "queued"},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	mux.HandleFunc("/repos/testorg/repo1/actions/runs/100/jobs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 2,
			"jobs": []map[string]any{
				{
					"id":     601,
					"status": "in_progress",
					"labels": []string{"self-hosted", "linux"},
				},
				{
					"id":     602,
					"status": "queued",
					"labels": []string{"self-hosted", "linux"},
				},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	events, err := c.PollJobs(context.Background())
	if err != nil {
		t.Fatalf("PollJobs() error: %v", err)
	}

	// Only the queued job should be returned, not the in_progress one
	if len(events) != 1 {
		t.Fatalf("PollJobs() returned %d events, want 1", len(events))
	}
	if events[0].Job.GetID() != 602 {
		t.Errorf("event job ID = %d, want 602 (only queued job)", events[0].Job.GetID())
	}
}

func TestPollJobs_OrgLevel_PartialRepoFailure(t *testing.T) {
	mux := http.NewServeMux()

	// Mock: list org repos returns two repos
	mux.HandleFunc("/orgs/testorg/repos", func(w http.ResponseWriter, r *http.Request) {
		repos := []map[string]any{
			{"id": 1, "name": "good-repo", "full_name": "testorg/good-repo"},
			{"id": 2, "name": "bad-repo", "full_name": "testorg/bad-repo"},
		}
		if err := json.NewEncoder(w).Encode(repos); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	// good-repo has a queued job
	mux.HandleFunc("/repos/testorg/good-repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 700, "status": "queued"},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	mux.HandleFunc("/repos/testorg/good-repo/actions/runs/700/jobs", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total_count": 1,
			"jobs": []map[string]any{
				{"id": 701, "status": "queued", "labels": []string{"self-hosted"}},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Logf("encoding: %v", err)
		}
	})

	// bad-repo returns 500
	mux.HandleFunc("/repos/testorg/bad-repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		if _, err := w.Write([]byte(`{"message":"error"}`)); err != nil {
			t.Logf("writing: %v", err)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()
	c.cfg.Repos = nil

	events, err := c.PollJobs(context.Background())
	if err != nil {
		t.Fatalf("PollJobs() should not error on partial org repo failure, got: %v", err)
	}

	// Should still get the job from good-repo
	if len(events) != 1 {
		t.Fatalf("PollJobs() returned %d events, want 1", len(events))
	}
	if events[0].Job.GetID() != 701 {
		t.Errorf("event job ID = %d, want 701", events[0].Job.GetID())
	}
}

// --- isSelfHosted edge case tests ---

func TestIsSelfHosted_EdgeCases(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"empty string label", []string{""}, false},
		{"whitespace label", []string{" self-hosted "}, false},
		{"many labels without self-hosted", []string{"ubuntu-22.04", "linux", "x64", "gpu", "large"}, false},
		{"self-hosted duplicated", []string{"self-hosted", "self-hosted"}, true},
		{"single non-matching label", []string{"macos-latest"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSelfHosted(tt.labels); got != tt.want {
				t.Errorf("isSelfHosted(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}
