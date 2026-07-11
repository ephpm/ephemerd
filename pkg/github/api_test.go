package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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
		// RegisterWebhooks first lists existing hooks (idempotency check).
		// Return an empty list so the create path runs.
		if r.Method == http.MethodGet {
			if err := json.NewEncoder(w).Encode([]map[string]any{}); err != nil {
				t.Logf("encoding: %v", err)
			}
			return
		}
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

// TestRegisterWebhooks_Idempotent_RepoLevel asserts that when a repo already
// has a hook pointing at the target URL, RegisterWebhooks reuses it and does
// NOT issue a create (which GitHub would reject with 422). This is the safety
// property the external-tunnel path depends on: it re-registers on every start.
func TestRegisterWebhooks_Idempotent_RepoLevel(t *testing.T) {
	const url = "https://tunnel.example.com/webhook"
	var createCalls atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/repo1/hooks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// A hook already exists for this URL.
			resp := []map[string]any{{
				"id":     int64(555),
				"config": map[string]any{"url": url},
			}}
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Logf("encoding: %v", err)
			}
		case http.MethodPost:
			createCalls.Add(1)
			w.WriteHeader(201)
			if err := json.NewEncoder(w).Encode(map[string]any{"id": 999}); err != nil {
				t.Logf("encoding: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	hooks, err := c.RegisterWebhooks(context.Background(), url, "secret")
	if err != nil {
		t.Fatalf("RegisterWebhooks() error: %v", err)
	}
	if n := createCalls.Load(); n != 0 {
		t.Errorf("create was called %d times, want 0 (existing hook should be reused)", n)
	}
	if len(hooks) != 1 {
		t.Fatalf("got %d hooks, want 1", len(hooks))
	}
	if hooks[0].HookID != 555 {
		t.Errorf("hook ID = %d, want 555 (the existing hook)", hooks[0].HookID)
	}
	if hooks[0].Repo != "repo1" {
		t.Errorf("hook repo = %q, want %q", hooks[0].Repo, "repo1")
	}
}

// TestRegisterWebhooks_Idempotent_DifferentURLCreates asserts that an existing
// hook with a DIFFERENT URL does not suppress creation of the one we want.
func TestRegisterWebhooks_Idempotent_DifferentURLCreates(t *testing.T) {
	const wantURL = "https://tunnel.example.com/webhook"
	var createCalls atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/repo1/hooks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			resp := []map[string]any{{
				"id":     int64(1),
				"config": map[string]any{"url": "https://other.example.com/webhook"},
			}}
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Logf("encoding: %v", err)
			}
		case http.MethodPost:
			createCalls.Add(1)
			w.WriteHeader(201)
			if err := json.NewEncoder(w).Encode(map[string]any{"id": 777}); err != nil {
				t.Logf("encoding: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	hooks, err := c.RegisterWebhooks(context.Background(), wantURL, "secret")
	if err != nil {
		t.Fatalf("RegisterWebhooks() error: %v", err)
	}
	if n := createCalls.Load(); n != 1 {
		t.Errorf("create was called %d times, want 1 (existing hook has a different URL)", n)
	}
	if len(hooks) != 1 || hooks[0].HookID != 777 {
		t.Fatalf("hooks = %+v, want one hook with ID 777", hooks)
	}
}

// TestRegisterWebhooks_Idempotent_ListErrorFallsBackToCreate asserts that when
// the list-existing-hooks call fails, RegisterWebhooks still attempts a create
// rather than silently doing nothing — the create then surfaces any real error.
func TestRegisterWebhooks_Idempotent_ListErrorFallsBackToCreate(t *testing.T) {
	var createCalls atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/repo1/hooks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusInternalServerError)
		case http.MethodPost:
			createCalls.Add(1)
			w.WriteHeader(201)
			if err := json.NewEncoder(w).Encode(map[string]any{"id": 42}); err != nil {
				t.Logf("encoding: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()

	hooks, err := c.RegisterWebhooks(context.Background(), "https://tunnel.example.com/webhook", "secret")
	if err != nil {
		t.Fatalf("RegisterWebhooks() error: %v", err)
	}
	if n := createCalls.Load(); n != 1 {
		t.Errorf("create was called %d times, want 1 (list error should fall back to create)", n)
	}
	if len(hooks) != 1 || hooks[0].HookID != 42 {
		t.Fatalf("hooks = %+v, want one hook with ID 42", hooks)
	}
}

// TestRegisterWebhooks_Idempotent_OrgLevel mirrors the repo-level idempotency
// test for the org-level path.
func TestRegisterWebhooks_Idempotent_OrgLevel(t *testing.T) {
	const url = "https://tunnel.example.com/webhook"
	var createCalls atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/testorg/hooks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			resp := []map[string]any{{
				"id":     int64(321),
				"config": map[string]any{"url": url},
			}}
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Logf("encoding: %v", err)
			}
		case http.MethodPost:
			createCalls.Add(1)
			w.WriteHeader(201)
			if err := json.NewEncoder(w).Encode(map[string]any{"id": 888}); err != nil {
				t.Logf("encoding: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()
	c.cfg.Repos = nil // org-level

	hooks, err := c.RegisterWebhooks(context.Background(), url, "secret")
	if err != nil {
		t.Fatalf("RegisterWebhooks() error: %v", err)
	}
	if n := createCalls.Load(); n != 0 {
		t.Errorf("create was called %d times, want 0 (existing org hook should be reused)", n)
	}
	if len(hooks) != 1 || hooks[0].HookID != 321 {
		t.Fatalf("hooks = %+v, want one hook with ID 321", hooks)
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

// TestRegisterWebhooks_RollbackOnPartialFailure asserts that when webhook
// creation fails partway through a multi-repo registration, every webhook
// already created (in earlier repos) is deleted before the function returns
// the error. Without rollback, ephemerd would leak hooks toward GitHub's
// per-org/repo cap on every crashed start-up attempt.
func TestRegisterWebhooks_RollbackOnPartialFailure(t *testing.T) {
	var (
		mu              sync.Mutex
		createdHookIDs        = map[string]int64{} // repo -> assigned hook id
		deleted               = map[string]bool{}  // repo:hookID -> true
		nextHookID      int64 = 100
		failOnRepo            = "repo3" // 3rd repo fails
		gotCreateOrder  []string
		gotDeletedOrder []string
	)

	mux := http.NewServeMux()
	for _, repo := range []string{"repo1", "repo2", "repo3", "repo4"} {
		repo := repo
		mux.HandleFunc("/repos/testorg/"+repo+"/hooks", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				mu.Lock()
				gotCreateOrder = append(gotCreateOrder, repo)
				mu.Unlock()

				if repo == failOnRepo {
					w.WriteHeader(http.StatusUnprocessableEntity)
					if _, err := w.Write([]byte(`{"message":"Validation Failed"}`)); err != nil {
						t.Logf("writing: %v", err)
					}
					return
				}

				mu.Lock()
				nextHookID++
				id := nextHookID
				createdHookIDs[repo] = id
				mu.Unlock()
				w.WriteHeader(201)
				if err := json.NewEncoder(w).Encode(map[string]any{"id": id}); err != nil {
					t.Logf("encoding: %v", err)
				}
				return
			}
			http.NotFound(w, r)
		})
	}

	// Each created hook is deleted via DELETE /repos/<owner>/<repo>/hooks/<id>.
	// Use a wildcard catch-all and parse the URL to track which were deleted.
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		// /repos/testorg/<repo>/hooks/<id>
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 5 || parts[3] != "hooks" {
			http.NotFound(w, r)
			return
		}
		repo := parts[2]
		hookID := parts[4]
		mu.Lock()
		deleted[repo+":"+hookID] = true
		gotDeletedOrder = append(gotDeletedOrder, repo)
		mu.Unlock()
		w.WriteHeader(204)
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()
	c.cfg.Repos = []string{"repo1", "repo2", "repo3", "repo4"}

	hooks, err := c.RegisterWebhooks(context.Background(), "https://tunnel.example.com/webhook", "secret")
	if err == nil {
		t.Fatal("expected error from partial-failure registration")
	}
	if hooks != nil {
		t.Errorf("hooks = %v, want nil on rollback error", hooks)
	}

	// Creation should have stopped at repo3 — repo4 must NOT have been
	// attempted.
	mu.Lock()
	defer mu.Unlock()
	if len(gotCreateOrder) != 3 {
		t.Fatalf("create order = %v, want exactly [repo1 repo2 repo3]", gotCreateOrder)
	}
	for i, repo := range []string{"repo1", "repo2", "repo3"} {
		if gotCreateOrder[i] != repo {
			t.Errorf("create order[%d] = %q, want %q", i, gotCreateOrder[i], repo)
		}
	}

	// Both successful hooks (repo1, repo2) must have been deleted.
	if !deleted["repo1:"+itoa(createdHookIDs["repo1"])] {
		t.Errorf("repo1 hook was not deleted on rollback")
	}
	if !deleted["repo2:"+itoa(createdHookIDs["repo2"])] {
		t.Errorf("repo2 hook was not deleted on rollback")
	}
	// repo3 itself never returned a hook (the create failed) so nothing to delete.
	if got := len(gotDeletedOrder); got != 2 {
		t.Errorf("got %d delete calls, want 2 (repo1 + repo2)", got)
	}
}

// TestRegisterWebhooks_RollbackContinuesOnDeleteFailure asserts that even
// if cleaning up an already-created webhook fails (network blip, GitHub 5xx),
// the loop keeps trying to clean up the rest. We don't want a single failed
// delete to abort the rollback and leak the remaining hooks.
func TestRegisterWebhooks_RollbackContinuesOnDeleteFailure(t *testing.T) {
	var (
		mu             sync.Mutex
		deleteAttempts atomic.Int32
		createdRepos   []string
		deletedRepos   []string
	)

	mux := http.NewServeMux()
	repos := []string{"alpha", "beta", "gamma"}
	for _, repo := range repos {
		repo := repo
		mux.HandleFunc("/repos/testorg/"+repo+"/hooks", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.NotFound(w, r)
				return
			}
			mu.Lock()
			createdRepos = append(createdRepos, repo)
			mu.Unlock()

			if repo == "gamma" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				if _, err := w.Write([]byte(`{"message":"Validation Failed"}`)); err != nil {
					t.Logf("writing: %v", err)
				}
				return
			}

			// alpha → hook id 1, beta → hook id 2
			id := int64(1)
			if repo == "beta" {
				id = 2
			}
			w.WriteHeader(201)
			if err := json.NewEncoder(w).Encode(map[string]any{"id": id}); err != nil {
				t.Logf("encoding: %v", err)
			}
		})
	}

	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		deleteAttempts.Add(1)
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 5 || parts[3] != "hooks" {
			http.NotFound(w, r)
			return
		}
		repo := parts[2]

		// First delete (alpha) fails — second delete (beta) must still happen.
		if repo == "alpha" {
			w.WriteHeader(500)
			return
		}

		mu.Lock()
		deletedRepos = append(deletedRepos, repo)
		mu.Unlock()
		w.WriteHeader(204)
	})

	c, srv := newTestClientWithServer(t, mux)
	defer srv.Close()
	c.cfg.Repos = repos

	if _, err := c.RegisterWebhooks(context.Background(), "https://tunnel.example.com/webhook", "secret"); err == nil {
		t.Fatal("expected error from partial-failure registration")
	}

	// Both successful creates must have triggered delete attempts (2 attempts).
	if got := deleteAttempts.Load(); got != 2 {
		t.Errorf("delete attempts = %d, want 2", got)
	}

	mu.Lock()
	defer mu.Unlock()
	// beta should be in the deleted list (alpha's delete failed).
	found := false
	for _, r := range deletedRepos {
		if r == "beta" {
			found = true
		}
	}
	if !found {
		t.Errorf("beta should have been deleted even after alpha's delete failed; deleted=%v", deletedRepos)
	}
	if len(createdRepos) != 3 {
		t.Errorf("create attempts = %v, want all three repos attempted", createdRepos)
	}
}

// itoa is a tiny helper to avoid importing strconv just for one integer
// formatting call in the rollback test.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
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
