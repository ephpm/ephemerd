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
