package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ephpm/ephemerd/pkg/metrics"
	gh "github.com/google/go-github/v72/github"
	"gopkg.in/yaml.v3"
)

// Config for the GitHub client.
type Config struct {
	Token string
	Owner string
	Repos []string
	Log   *slog.Logger

	// App auth (used instead of Token when set)
	AppAuth *AppAuth

	// PoolMode changes webhook lifecycle semantics for nodes sharing one
	// public webhook URL: registration adopts an existing same-URL hook,
	// deregistration is a no-op, and the stale-hook sweep is skipped.
	PoolMode bool
}

// Client handles GitHub API interactions and webhook events.
type Client struct {
	cfg      Config
	client   *gh.Client
	app      *AppAuth      // nil when using PAT
	lastRate *rateSnapshot // last observed rate-limit state (nil in tests using SetHTTPClient)
}

// RateSnapshot returns the last-observed rate-limit state. The scheduler
// uses this to bias retry backoff — when remaining==0 and now < reset,
// the next attempt is scheduled just after reset instead of blindly
// falling through the exponential-backoff ladder. Fields are zero-valued
// when the client hasn't yet made a request or was created via the
// bare test constructor.
func (c *Client) RateSnapshot() (remaining, limit int64, reset, updated time.Time) {
	if c.lastRate == nil {
		return 0, 0, time.Time{}, time.Time{}
	}
	return c.lastRate.Snapshot()
}

// JobEvent is emitted when a workflow_job webhook fires.
type JobEvent struct {
	Action string
	Repo   string
	Job    *gh.WorkflowJob
}

// New creates a GitHub client.
// Uses AppAuth for dynamic token refresh when configured, otherwise a static PAT.
//
// All GitHub API responses flow through rateTrackingTransport, which parses
// the X-RateLimit-* headers and updates the ephemerd_github_api_rate_*
// gauges. Operators use ephemerd_github_api_rate_updated_seconds to tell a
// current-but-exhausted budget from a stale reading.
func New(cfg Config) (*Client, error) {
	var c *gh.Client
	var lastRate rateSnapshot
	if cfg.AppAuth != nil {
		// Use a custom transport that injects the latest token on each
		// request, then wrap it in the rate tracker so every response
		// updates the metrics.
		httpClient := &http.Client{
			Transport: &rateTrackingTransport{
				next: &appAuthTransport{app: cfg.AppAuth},
				last: &lastRate,
			},
		}
		c = gh.NewClient(httpClient)
	} else {
		httpClient := &http.Client{
			Transport: &rateTrackingTransport{
				next: http.DefaultTransport,
				last: &lastRate,
			},
		}
		c = gh.NewClient(httpClient).WithAuthToken(cfg.Token)
	}

	return &Client{
		cfg:      cfg,
		client:   c,
		app:      cfg.AppAuth,
		lastRate: &lastRate,
	}, nil
}

// appAuthTransport injects the current App installation token into each request.
type appAuthTransport struct {
	app *AppAuth
}

func (t *appAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "token "+t.app.Token())
	return http.DefaultTransport.RoundTrip(req)
}

// rateSnapshot is the last observed GitHub rate-limit state. All fields
// are stored as int64 unix seconds / counts so atomic loads and stores
// are lock-free.
type rateSnapshot struct {
	remaining   atomic.Int64 // X-RateLimit-Remaining
	limit       atomic.Int64 // X-RateLimit-Limit
	resetUnix   atomic.Int64 // X-RateLimit-Reset (unix seconds)
	updatedUnix atomic.Int64 // when we last parsed rate headers
}

// Snapshot returns a point-in-time copy for use by the scheduler retry
// queue (rate-aware backoff).
func (r *rateSnapshot) Snapshot() (remaining, limit int64, reset, updated time.Time) {
	remaining = r.remaining.Load()
	limit = r.limit.Load()
	if u := r.resetUnix.Load(); u > 0 {
		reset = time.Unix(u, 0)
	}
	if u := r.updatedUnix.Load(); u > 0 {
		updated = time.Unix(u, 0)
	}
	return remaining, limit, reset, updated
}

// rateTrackingTransport wraps another RoundTripper and updates the rate
// gauges + a shared rateSnapshot from response headers on every call.
type rateTrackingTransport struct {
	next http.RoundTripper
	last *rateSnapshot
}

func (t *rateTrackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if resp != nil {
		t.observe(resp)
	}
	return resp, err
}

// observe parses X-RateLimit-* headers from a response and updates
// metrics + the shared rateSnapshot. Called on every response, including
// error responses (429/5xx) — those still carry the rate headers.
func (t *rateTrackingTransport) observe(resp *http.Response) {
	rem := resp.Header.Get("X-RateLimit-Remaining")
	if rem == "" {
		// Not every endpoint returns rate headers (e.g. some GraphQL
		// paths). Don't touch the gauge — a fresher value is better
		// than clobbering with 0.
		return
	}
	if v, err := strconv.ParseInt(rem, 10, 64); err == nil {
		t.last.remaining.Store(v)
		metrics.GitHubAPIRateRemaining.Set(float64(v))
	}
	if lim := resp.Header.Get("X-RateLimit-Limit"); lim != "" {
		if v, err := strconv.ParseInt(lim, 10, 64); err == nil {
			t.last.limit.Store(v)
			metrics.GitHubAPIRateLimit.Set(float64(v))
		}
	}
	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		if v, err := strconv.ParseInt(reset, 10, 64); err == nil {
			t.last.resetUnix.Store(v)
			metrics.GitHubAPIRateResetSeconds.Set(float64(v))
		}
	}
	now := time.Now().Unix()
	t.last.updatedUnix.Store(now)
	metrics.GitHubAPIRateUpdatedSeconds.Set(float64(now))
}

// SetHTTPClient replaces the underlying go-github client.
// Used by test infrastructure to point at a fake server.
func (c *Client) SetHTTPClient(ghClient *gh.Client) {
	c.client = ghClient
}

// IsOrgLevel returns true when no repos are configured, meaning ephemerd
// registers runners at the organization level (available to all repos).
func (c *Client) IsOrgLevel() bool {
	return len(c.cfg.Repos) == 0
}

// RegisterJITRunner creates a just-in-time runner.
// If repos are configured, registers at the repo level.
// If repos are empty, registers at the org level (available to all repos in the org).
func (c *Client) RegisterJITRunner(ctx context.Context, repo string, name string, labels []string) (*gh.JITRunnerConfig, error) {
	req := &gh.GenerateJITConfigRequest{
		Name:          name,
		RunnerGroupID: 1, // default group
		Labels:        labels,
	}

	var config *gh.JITRunnerConfig
	var err error

	if c.IsOrgLevel() {
		config, _, err = c.client.Actions.GenerateOrgJITConfig(ctx, c.cfg.Owner, req)
		if err != nil {
			return nil, fmt.Errorf("generating org JIT config for %s: %w", c.cfg.Owner, err)
		}
		c.cfg.Log.Info("registered org-level JIT runner", "name", name, "labels", labels)
	} else {
		config, _, err = c.client.Actions.GenerateRepoJITConfig(ctx, c.cfg.Owner, repo, req)
		if err != nil {
			return nil, fmt.Errorf("generating JIT config for %s/%s: %w", c.cfg.Owner, repo, err)
		}
		c.cfg.Log.Info("registered repo-level JIT runner", "repo", repo, "name", name, "labels", labels)
	}

	return config, nil
}

// RemoveRunner removes a self-hosted runner by ID.
// Uses org-level or repo-level API depending on configuration.
func (c *Client) RemoveRunner(ctx context.Context, repo string, runnerID int64) error {
	var err error
	if c.IsOrgLevel() {
		_, err = c.client.Actions.RemoveOrganizationRunner(ctx, c.cfg.Owner, runnerID)
	} else {
		_, err = c.client.Actions.RemoveRunner(ctx, c.cfg.Owner, repo, runnerID)
	}
	if err != nil {
		return fmt.Errorf("removing runner %d: %w", runnerID, err)
	}
	return nil
}

// FetchJobImage fetches the workflow run's job definition and reads the
// container image declared in the job's `container:` field. This requires an
// extra API call per job but lets users specify the image directly in their
// workflow:
//
//	jobs:
//	  build:
//	    runs-on: [self-hosted, linux, x64]
//	    container:
//	      image: ghcr.io/myorg/custom-build:latest
//	  quick:
//	    runs-on: [self-hosted, linux, x64]
//	    container: ghcr.io/myorg/quick:latest  # shorthand
//
// Returns empty string if no container image is set.
func (c *Client) FetchJobImage(ctx context.Context, repo string, runID int64, jobID int64) string {
	// The Jobs API only gives us IDs and names — we resolve the target job's
	// name here, then match it against the workflow YAML below.
	jobs, _, err := c.client.Actions.ListWorkflowJobs(ctx, c.cfg.Owner, repo, runID, nil)
	if err != nil {
		c.cfg.Log.Debug("failed to fetch workflow jobs for image lookup", "error", err)
		return ""
	}

	run, _, err := c.client.Actions.GetWorkflowRunByID(ctx, c.cfg.Owner, repo, runID)
	if err != nil {
		c.cfg.Log.Debug("failed to fetch workflow run for image lookup", "error", err)
		return ""
	}

	workflowPath := run.GetPath()
	if workflowPath == "" {
		return ""
	}

	fileContent, _, _, err := c.client.Repositories.GetContents(ctx, c.cfg.Owner, repo, workflowPath, &gh.RepositoryContentGetOptions{
		Ref: run.GetHeadSHA(),
	})
	if err != nil || fileContent == nil {
		c.cfg.Log.Debug("failed to fetch workflow file for image lookup", "path", workflowPath, "error", err)
		return ""
	}

	content, err := fileContent.GetContent()
	if err != nil {
		return ""
	}

	image := parseContainerImage(content, jobs.Jobs, jobID)
	if image != "" {
		c.cfg.Log.Info("job specifies custom image", "job_id", jobID, "image", image)
	}

	return image
}

// workflowSchema is the subset of a GitHub Actions workflow YAML we need to parse.
type workflowSchema struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Name      string         `yaml:"name"`
	Container containerField `yaml:"container"`
}

// containerField accepts either a string shorthand or a full mapping for the
// job's `container:` key, matching GitHub Actions' own parser.
type containerField struct {
	Image string `yaml:"image"`
}

func (c *containerField) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		c.Image = node.Value
		return nil
	case yaml.MappingNode:
		type raw containerField
		var r raw
		if err := node.Decode(&r); err != nil {
			return err
		}
		*c = containerField(r)
		return nil
	default:
		return fmt.Errorf("container: expected string or mapping, got %v", node.Kind)
	}
}

// parseContainerImage extracts container.image from a workflow YAML for a
// specific job.
func parseContainerImage(workflowContent string, jobs []*gh.WorkflowJob, targetJobID int64) string {
	var jobName string
	for _, j := range jobs {
		if j.GetID() == targetJobID {
			jobName = j.GetName()
			break
		}
	}
	if jobName == "" {
		return ""
	}

	var wf workflowSchema
	if err := yaml.Unmarshal([]byte(workflowContent), &wf); err != nil {
		return ""
	}

	for key, job := range wf.Jobs {
		name := job.Name
		if name == "" {
			name = key
		}
		if name == jobName {
			return job.Container.Image
		}
	}

	return ""
}

// PollJobs checks for queued workflow jobs targeting self-hosted runners.
// If repos are configured, polls those repos. Otherwise, polls all repos in the org.
func (c *Client) PollJobs(ctx context.Context) ([]JobEvent, error) {
	repos := c.cfg.Repos

	// Org-level: discover repos with queued runs
	if len(repos) == 0 {
		return c.pollOrg(ctx)
	}

	var events []JobEvent
	for _, repo := range repos {
		repoEvents, err := c.pollRepo(ctx, repo)
		if err != nil {
			c.cfg.Log.Warn("failed to poll repo", "repo", repo, "error", err)
			continue
		}
		events = append(events, repoEvents...)
	}
	return events, nil
}

// pollOrg discovers queued workflow runs across the entire org.
func (c *Client) pollOrg(ctx context.Context) ([]JobEvent, error) {
	var events []JobEvent

	// List all repos in the org and check each for queued runs.
	// TODO: GitHub doesn't have an org-level "list all queued jobs" API,
	// so we list repos and poll each. This could be optimized with caching.
	repos, _, err := c.client.Repositories.ListByOrg(ctx, c.cfg.Owner, &gh.RepositoryListByOrgOptions{
		Type:        "all",
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return nil, fmt.Errorf("listing org repos: %w", err)
	}

	for _, repo := range repos {
		repoEvents, err := c.pollRepo(ctx, repo.GetName())
		if err != nil {
			c.cfg.Log.Debug("failed to poll repo", "repo", repo.GetName(), "error", err)
			continue
		}
		events = append(events, repoEvents...)
	}

	return events, nil
}

// pollRepo checks a single repo for queued workflow jobs.
func (c *Client) pollRepo(ctx context.Context, repo string) ([]JobEvent, error) {
	var events []JobEvent

	runs, _, err := c.client.Actions.ListRepositoryWorkflowRuns(ctx, c.cfg.Owner, repo, &gh.ListWorkflowRunsOptions{
		Status: "queued",
	})
	if err != nil {
		return nil, err
	}

	for _, run := range runs.WorkflowRuns {
		runJobs, _, err := c.client.Actions.ListWorkflowJobs(ctx, c.cfg.Owner, repo, run.GetID(), nil)
		if err != nil {
			continue
		}
		for _, job := range runJobs.Jobs {
			if job.GetStatus() == "queued" && isSelfHosted(job.Labels) {
				events = append(events, JobEvent{
					Action: "queued",
					Repo:   repo,
					Job:    job,
				})
			}
		}
	}

	return events, nil
}

// WebhookHandler returns an http.Handler that processes workflow_job webhook events.
// Events are sent to the returned channel.
func (c *Client) WebhookHandler(secret string) (http.Handler, <-chan JobEvent) {
	events := make(chan JobEvent, 32)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Verify webhook signature
		if secret != "" {
			sig := r.Header.Get("X-Hub-Signature-256")
			if !verifySignature(body, sig, secret) {
				c.cfg.Log.Warn("webhook signature verification failed")
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		eventType := r.Header.Get("X-GitHub-Event")
		if eventType != "workflow_job" {
			w.WriteHeader(http.StatusOK)
			return
		}

		var payload struct {
			Action      string          `json:"action"`
			WorkflowJob *gh.WorkflowJob `json:"workflow_job"`
			Repository  *gh.Repository  `json:"repository"`
		}

		if err := json.Unmarshal(body, &payload); err != nil {
			c.cfg.Log.Error("failed to parse webhook payload", "error", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if payload.WorkflowJob == nil || payload.Repository == nil {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Only handle jobs targeting self-hosted runners
		if !isSelfHosted(payload.WorkflowJob.Labels) {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Only handle repos we're configured for
		repoName := payload.Repository.GetName()
		if !c.isTrackedRepo(repoName) {
			w.WriteHeader(http.StatusOK)
			return
		}

		c.cfg.Log.Info("webhook event",
			"action", payload.Action,
			"repo", repoName,
			"job_id", payload.WorkflowJob.GetID(),
			"labels", payload.WorkflowJob.Labels,
			"runner_name", payload.WorkflowJob.GetRunnerName(),
		)

		events <- JobEvent{
			Action: payload.Action,
			Repo:   repoName,
			Job:    payload.WorkflowJob,
		}

		w.WriteHeader(http.StatusOK)
	})

	return handler, events
}

func (c *Client) isTrackedRepo(repo string) bool {
	// Org-level: accept all repos
	if len(c.cfg.Repos) == 0 {
		return true
	}
	for _, r := range c.cfg.Repos {
		if r == repo {
			return true
		}
	}
	return false
}

func isSelfHosted(labels []string) bool {
	for _, l := range labels {
		if l == "self-hosted" {
			return true
		}
	}
	return false
}

func verifySignature(body []byte, signature string, secret string) bool {
	if len(signature) < 7 || signature[:7] != "sha256=" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(signature[7:]), []byte(expected))
}
