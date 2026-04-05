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

	gh "github.com/google/go-github/v72/github"
)

// Config for the GitHub client.
type Config struct {
	Token string
	Owner string
	Repos []string
	Log   *slog.Logger
}

// Client handles GitHub API interactions and webhook events.
type Client struct {
	cfg    Config
	client *gh.Client
}

// JobEvent is emitted when a workflow_job webhook fires.
type JobEvent struct {
	Action string
	Repo   string
	Job    *gh.WorkflowJob
}

// New creates a GitHub client.
func New(cfg Config) (*Client, error) {
	c := gh.NewClient(nil).WithAuthToken(cfg.Token)

	return &Client{
		cfg:    cfg,
		client: c,
	}, nil
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

// FetchJobImage fetches the workflow run's job definition and looks for an
// EPHEMERD_IMAGE environment variable. This requires an extra API call per job
// but allows users to specify the container image directly in their workflow:
//
//	jobs:
//	  build:
//	    runs-on: [self-hosted, linux, x64]
//	    env:
//	      EPHEMERD_IMAGE: ghcr.io/myorg/custom-build:latest
//
// Returns empty string if no EPHEMERD_IMAGE is set.
func (c *Client) FetchJobImage(ctx context.Context, repo string, runID int64, jobID int64) string {
	// Fetch the workflow file content to read job-level env vars.
	// The Jobs API doesn't expose env, so we fetch via the workflow run.
	jobs, _, err := c.client.Actions.ListWorkflowJobs(ctx, c.cfg.Owner, repo, runID, nil)
	if err != nil {
		c.cfg.Log.Debug("failed to fetch workflow jobs for image lookup", "error", err)
		return ""
	}

	for _, job := range jobs.Jobs {
		if job.GetID() != jobID {
			continue
		}

		// The Jobs API doesn't directly expose env vars from the workflow YAML.
		// However, we can read them from the workflow file via the run's workflow path.
		// For now, check if the job name encodes an image hint as a convention,
		// or fetch the workflow YAML from the repo.
		break
	}

	// Fetch the workflow YAML to read the job's env block
	run, _, err := c.client.Actions.GetWorkflowRunByID(ctx, c.cfg.Owner, repo, runID)
	if err != nil {
		c.cfg.Log.Debug("failed to fetch workflow run for image lookup", "error", err)
		return ""
	}

	// Get the workflow file from the repo at the run's head SHA
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

	// Parse the YAML to find the job's EPHEMERD_IMAGE env var.
	// We do a lightweight parse — look for the job by name and extract env.
	image := parseEphemerdImage(content, jobs.Jobs, jobID)
	if image != "" {
		c.cfg.Log.Info("job specifies custom image", "job_id", jobID, "image", image)
	}

	return image
}

// parseEphemerdImage extracts EPHEMERD_IMAGE from a workflow YAML for a specific job.
// Uses simple string matching to avoid pulling in a full YAML parser dependency.
func parseEphemerdImage(workflowContent string, jobs []*gh.WorkflowJob, targetJobID int64) string {
	// Find the target job's name
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

	// Simple line-by-line scan for EPHEMERD_IMAGE in env blocks.
	// This is intentionally simple — a full YAML parser is overkill for
	// extracting one env var, and we don't want the dependency.
	//
	// Strategy: scan each job block for a name: field. If name matches jobName,
	// or if the job key itself matches jobName (when no name: field), look for
	// EPHEMERD_IMAGE in that job's env block.
	lines := splitLines(workflowContent)
	inJobs := false

	// First pass: collect per-job data (key, name, EPHEMERD_IMAGE)
	type jobBlock struct {
		key   string // the YAML key (e.g. "build")
		name  string // from name: field, or empty
		image string // EPHEMERD_IMAGE value, or empty
	}
	var blocks []jobBlock
	var current *jobBlock
	inEnv := false

	for _, line := range lines {
		trimmed := trimSpace(line)
		indent := len(line) - len(trimLeft(line))

		if trimmed == "jobs:" {
			inJobs = true
			continue
		}

		if inJobs && indent == 2 && len(trimmed) > 0 && trimmed[len(trimmed)-1] == ':' {
			// New job key
			if current != nil {
				blocks = append(blocks, *current)
			}
			key := trimmed[:len(trimmed)-1]
			current = &jobBlock{key: key}
			inEnv = false
			continue
		}

		if current == nil {
			continue
		}

		// Detect leaving the job block
		if inJobs && indent <= 2 && trimmed != "" && (indent < 2 || trimmed[len(trimmed)-1] != ':') {
			// Left jobs section entirely
			break
		}

		if indent == 4 && hasPrefix(trimmed, "name:") {
			current.name = stripQuotes(trimSpace(trimmed[5:]))
		}

		if indent == 4 && trimmed == "env:" {
			inEnv = true
			continue
		}

		if inEnv && indent == 6 {
			if hasPrefix(trimmed, "EPHEMERD_IMAGE:") {
				current.image = stripQuotes(trimSpace(trimmed[len("EPHEMERD_IMAGE:"):]))
			}
			continue
		}

		if inEnv && indent <= 4 {
			inEnv = false
		}
	}
	if current != nil {
		blocks = append(blocks, *current)
	}

	// Match: name field takes precedence, fall back to key
	for _, b := range blocks {
		displayName := b.name
		if displayName == "" {
			displayName = b.key
		}
		if displayName == jobName && b.image != "" {
			return b.image
		}
	}

	return ""
}

// Simple string helpers to avoid importing strings package just for these.
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func trimLeft(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[i:]
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func stripQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
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
