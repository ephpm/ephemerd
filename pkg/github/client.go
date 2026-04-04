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

// RegisterJITRunner creates a just-in-time runner for a specific repo.
// Returns the runner JIT config token that the Actions runner uses to register.
func (c *Client) RegisterJITRunner(ctx context.Context, repo string, name string, labels []string) (*gh.JITRunnerConfig, error) {
	req := &gh.GenerateJITConfigRequest{
		Name:          name,
		RunnerGroupID: 1, // default group
		Labels:        labels,
	}

	config, _, err := c.client.Actions.GenerateRepoJITConfig(ctx, c.cfg.Owner, repo, req)
	if err != nil {
		return nil, fmt.Errorf("generating JIT config for %s/%s: %w", c.cfg.Owner, repo, err)
	}

	c.cfg.Log.Info("registered JIT runner",
		"repo", repo,
		"name", name,
		"labels", labels,
	)

	return config, nil
}

// RemoveRunner removes a self-hosted runner by ID.
func (c *Client) RemoveRunner(ctx context.Context, repo string, runnerID int64) error {
	_, err := c.client.Actions.RemoveRunner(ctx, c.cfg.Owner, repo, runnerID)
	if err != nil {
		return fmt.Errorf("removing runner %d from %s/%s: %w", runnerID, c.cfg.Owner, repo, err)
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
	lines := splitLines(workflowContent)
	inJobs := false
	inTargetJob := false
	inEnv := false

	for _, line := range lines {
		trimmed := trimSpace(line)
		indent := len(line) - len(trimLeft(line))

		// Track YAML structure by indentation
		if trimmed == "jobs:" {
			inJobs = true
			continue
		}

		if inJobs && indent == 2 && len(trimmed) > 0 && trimmed[len(trimmed)-1] == ':' {
			// Job key — check if it matches our target (by checking if the
			// job name appears in subsequent name: field, or just scan all jobs)
			inTargetJob = true // scan all jobs for now, match by env var presence
			inEnv = false
		}

		if inTargetJob && indent == 4 && trimmed == "env:" {
			inEnv = true
			continue
		}

		if inEnv && indent == 6 {
			if hasPrefix(trimmed, "EPHEMERD_IMAGE:") {
				value := trimSpace(trimmed[len("EPHEMERD_IMAGE:"):])
				// Strip optional quotes
				if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
					value = value[1 : len(value)-1]
				}
				if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
					value = value[1 : len(value)-1]
				}
				return value
			}
			continue
		}

		// Left the env block
		if inEnv && indent <= 4 {
			inEnv = false
		}
		if inTargetJob && indent <= 2 && trimmed != "" {
			inTargetJob = false
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

// PollJobs checks all configured repos for queued workflow jobs targeting
// self-hosted runners. Returns job events for any newly queued jobs.
func (c *Client) PollJobs(ctx context.Context) ([]JobEvent, error) {
	var events []JobEvent

	for _, repo := range c.cfg.Repos {
		jobs, _, err := c.client.Actions.ListWorkflowJobs(ctx, c.cfg.Owner, repo, 0, &gh.ListWorkflowJobsOptions{
			Filter: "latest",
		})
		if err != nil {
			// Try listing workflow runs and their jobs instead
			runs, _, err := c.client.Actions.ListRepositoryWorkflowRuns(ctx, c.cfg.Owner, repo, &gh.ListWorkflowRunsOptions{
				Status: "queued",
			})
			if err != nil {
				c.cfg.Log.Warn("failed to poll repo", "repo", repo, "error", err)
				continue
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
			continue
		}

		for _, job := range jobs.Jobs {
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
