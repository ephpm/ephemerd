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
