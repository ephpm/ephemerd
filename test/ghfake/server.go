// Package ghfake implements a fake GitHub REST API server for local e2e testing.
//
// The server implements the subset of the GitHub API that ephemerd's
// pkg/github.Client uses: PollJobs, RegisterJITRunner, RemoveRunner,
// and FetchJobImage. It runs as an httptest.Server and returns a
// configured github.Client pointing at it.
//
// Usage:
//
//	srv := ghfake.New("myorg")
//	defer srv.Close()
//	srv.QueueJob("myrepo", []string{"self-hosted", "linux", "x64"})
//
//	client := srv.Client()
//	events, _ := client.PollJobs(ctx)  // returns the queued job
//	jit, _ := client.RegisterJITRunner(ctx, "myrepo", "runner-1", labels)
//	// ... runner runs and exits ...
//	client.RemoveRunner(ctx, "myrepo", jit.GetRunner().GetID())
//	// srv.Removed(runnerID) == true
package ghfake

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/ephpm/ephemerd/pkg/github"
)

// job represents a queued workflow job.
type job struct {
	ID     int64
	RunID  int64
	Repo   string
	Labels []string
	Status string // "queued", "in_progress", "completed"
}

// runner tracks a registered JIT runner.
type runner struct {
	ID      int64
	Name    string
	Repo    string
	Removed bool
}

// Server is a fake GitHub REST API backed by httptest.Server.
type Server struct {
	*httptest.Server

	owner string

	mu       sync.Mutex
	jobs     map[int64]*job
	runners  map[int64]*runner
	removeCh chan int64 // signals runner removal

	nextJobID    atomic.Int64
	nextRunnerID atomic.Int64
	nextRunID    atomic.Int64
}

// New creates and starts a fake GitHub server for the given org/owner.
func New(owner string) *Server {
	s := &Server{
		owner:    owner,
		jobs:     make(map[int64]*job),
		runners:  make(map[int64]*runner),
		removeCh: make(chan int64, 64),
	}
	s.nextJobID.Store(100)
	s.nextRunnerID.Store(1000)
	s.nextRunID.Store(10)

	mux := http.NewServeMux()
	s.registerRoutes(mux)
	s.Server = httptest.NewServer(mux)
	return s
}

// QueueJob adds a job to the fake server's queue. Returns the job ID.
func (s *Server) QueueJob(repo string, labels []string) int64 {
	jobID := s.nextJobID.Add(1)
	runID := s.nextRunID.Add(1)

	s.mu.Lock()
	s.jobs[jobID] = &job{
		ID:     jobID,
		RunID:  runID,
		Repo:   repo,
		Labels: labels,
		Status: "queued",
	}
	s.mu.Unlock()
	return jobID
}

// Removed returns true if the runner with the given ID has been deregistered.
func (s *Server) Removed(runnerID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runners[runnerID]
	return ok && r.Removed
}

// WaitForRemoval blocks until the specified runner is removed or the timeout expires.
// Returns true if removal was detected, false on timeout.
func (s *Server) WaitForRemoval(runnerID int64, timeout time.Duration) bool {
	if s.Removed(runnerID) {
		return true
	}
	deadline := time.After(timeout)
	for {
		select {
		case id := <-s.removeCh:
			if id == runnerID {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// Client returns an ephemerd github.Client configured to talk to this fake server.
func (s *Server) Client() *github.Client {
	ghClient := gh.NewClient(nil).WithAuthToken("fake-token")
	u, _ := url.Parse(s.URL + "/")
	ghClient.BaseURL = u

	c, _ := github.New(github.Config{
		Token: "fake-token",
		Owner: s.owner,
		Repos: s.allRepos(),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// Replace the internal go-github client with ours that points at the fake.
	c.SetHTTPClient(ghClient)
	return c
}

// allRepos returns the set of repos that have queued jobs.
func (s *Server) allRepos() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]bool)
	for _, j := range s.jobs {
		seen[j.Repo] = true
	}
	repos := make([]string, 0, len(seen))
	for r := range seen {
		repos = append(repos, r)
	}
	return repos
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	// List workflow runs (PollJobs)
	// Pattern: /repos/{owner}/{repo}/actions/runs
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		s.routeRepos(w, r)
	})

	// Org-level JIT config
	// Pattern: /orgs/{owner}/actions/runners/generate-jitconfig
	mux.HandleFunc("/orgs/", func(w http.ResponseWriter, r *http.Request) {
		s.routeOrgs(w, r)
	})
}

func (s *Server) routeRepos(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// DELETE /repos/{owner}/{repo}/actions/runners/{id}
	if r.Method == http.MethodDelete && strings.Contains(path, "/actions/runners/") {
		s.handleRemoveRunner(w, r)
		return
	}

	// POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig
	if r.Method == http.MethodPost && strings.HasSuffix(path, "/actions/runners/generate-jitconfig") {
		s.handleGenerateJITConfig(w, r)
		return
	}

	// GET /repos/{owner}/{repo}/actions/runs/{id}/jobs
	if r.Method == http.MethodGet && strings.Contains(path, "/actions/runs/") && strings.HasSuffix(path, "/jobs") {
		s.handleListJobs(w, r)
		return
	}

	// GET /repos/{owner}/{repo}/actions/runs
	if r.Method == http.MethodGet && strings.HasSuffix(path, "/actions/runs") {
		s.handleListRuns(w, r)
		return
	}

	// GET /repos/{owner}/{repo}/contents/{path} (for FetchJobImage)
	if r.Method == http.MethodGet && strings.Contains(path, "/contents/") {
		s.handleGetContents(w, r)
		return
	}

	// GET /repos/{owner}/{repo}/actions/runs/{id} (for FetchJobImage)
	if r.Method == http.MethodGet && strings.Contains(path, "/actions/runs/") {
		s.handleGetRun(w, r)
		return
	}

	http.NotFound(w, r)
}

func (s *Server) routeOrgs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/runners/generate-jitconfig") {
		s.handleGenerateJITConfig(w, r)
		return
	}
	http.NotFound(w, r)
}

// handleListRuns returns queued workflow runs.
// GET /repos/{owner}/{repo}/actions/runs?status=queued
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	repo := extractRepo(r.URL.Path)

	s.mu.Lock()
	var runs []map[string]any
	seenRuns := make(map[int64]bool)
	for _, j := range s.jobs {
		if j.Repo == repo && j.Status == "queued" && !seenRuns[j.RunID] {
			seenRuns[j.RunID] = true
			runs = append(runs, map[string]any{
				"id":     j.RunID,
				"status": "queued",
				"path":   ".github/workflows/test.yml",
				"head_sha": "abc123",
			})
		}
	}
	s.mu.Unlock()

	writeJSON(w, map[string]any{
		"total_count":   len(runs),
		"workflow_runs": runs,
	})
}

// handleListJobs returns jobs for a workflow run.
// GET /repos/{owner}/{repo}/actions/runs/{id}/jobs
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	runID := extractRunID(r.URL.Path)

	s.mu.Lock()
	var jobs []map[string]any
	for _, j := range s.jobs {
		if j.RunID == runID {
			jobs = append(jobs, map[string]any{
				"id":     j.ID,
				"run_id": j.RunID,
				"status": j.Status,
				"labels": j.Labels,
				"name":   "test-job",
			})
		}
	}
	s.mu.Unlock()

	writeJSON(w, map[string]any{
		"total_count": len(jobs),
		"jobs":        jobs,
	})
}

// handleGenerateJITConfig creates a fake JIT runner config.
// POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig
func (s *Server) handleGenerateJITConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string   `json:"name"`
		RunnerGroupID int64    `json:"runner_group_id"`
		Labels        []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	runnerID := s.nextRunnerID.Add(1)

	s.mu.Lock()
	s.runners[runnerID] = &runner{
		ID:   runnerID,
		Name: req.Name,
		Repo: extractRepo(r.URL.Path),
	}
	s.mu.Unlock()

	// Build a plausible-looking JIT config. The runner binary won't be able
	// to connect to the Actions Service URL, but ephemerd just passes it
	// through — it never parses the contents.
	jitInner := map[string]string{
		".runner":               base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"agentId":%d,"agentName":"%s","serverUrl":"%s","workFolder":"_work"}`, runnerID, req.Name, s.URL))),
		".credentials":         base64.StdEncoding.EncodeToString([]byte(`{"scheme":"OAuth","data":{"clientId":"fake-client","authorizationUrl":"` + s.URL + `/auth"}}`)),
		".credentials_rsaparams": base64.StdEncoding.EncodeToString([]byte(`{"d":"fake","dp":"fake","dq":"fake","exponent":"AQAB","inverseQ":"fake","modulus":"fake","p":"fake","q":"fake"}`)),
	}
	innerJSON, _ := json.Marshal(jitInner)
	encodedJIT := base64.StdEncoding.EncodeToString(innerJSON)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{
		"runner": map[string]any{
			"id":   runnerID,
			"name": req.Name,
		},
		"encoded_jit_config": encodedJIT,
	})
}

// handleRemoveRunner deregisters a runner.
// DELETE /repos/{owner}/{repo}/actions/runners/{id}
func (s *Server) handleRemoveRunner(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad runner id", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if runner, ok := s.runners[id]; ok {
		runner.Removed = true
	}
	s.mu.Unlock()

	// Signal waiters.
	select {
	case s.removeCh <- id:
	default:
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleGetContents returns a fake workflow file (for FetchJobImage).
// GET /repos/{owner}/{repo}/contents/{path}
func (s *Server) handleGetContents(w http.ResponseWriter, r *http.Request) {
	// Return a minimal workflow YAML without EPHEMERD_IMAGE.
	content := base64.StdEncoding.EncodeToString([]byte(`name: test
on: push
jobs:
  test:
    runs-on: [self-hosted, linux]
    steps:
      - run: echo hello
`))

	writeJSON(w, map[string]any{
		"type":     "file",
		"encoding": "base64",
		"content":  content,
	})
}

// handleGetRun returns a fake workflow run (for FetchJobImage path lookup).
// GET /repos/{owner}/{repo}/actions/runs/{id}
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"id":       1,
		"status":   "queued",
		"path":     ".github/workflows/test.yml",
		"head_sha": "abc123",
	})
}

// --- helpers ---

// extractRepo extracts the repo name from a path like /repos/{owner}/{repo}/...
func extractRepo(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "repos" {
		return parts[2]
	}
	return ""
}

// extractRunID extracts the run ID from /repos/{owner}/{repo}/actions/runs/{id}/jobs
func extractRunID(path string) int64 {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, p := range parts {
		if p == "runs" && i+1 < len(parts) {
			id, _ := strconv.ParseInt(parts[i+1], 10, 64)
			return id
		}
	}
	return 0
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
