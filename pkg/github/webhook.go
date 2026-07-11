package github

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v72/github"
)

// ManagedWebhook tracks a webhook created by ephemerd so it can be cleaned up on shutdown.
type ManagedWebhook struct {
	Repo   string // empty for org-level
	HookID int64
}

// RegisterWebhooks creates workflow_job webhooks on each configured repo (or the org)
// pointing at the given URL with the given secret. Returns the managed hooks for cleanup.
//
// It is idempotent: before creating a hook it lists the existing hooks and, if
// one already points at webhookURL, reuses it instead of creating a duplicate.
// GitHub rejects a duplicate hook config with HTTP 422, so without this the
// external-tunnel path (which re-registers on every startup) would fail on the
// second boot. The list-then-skip approach makes repeated registration a no-op.
func (c *Client) RegisterWebhooks(ctx context.Context, webhookURL, secret string) ([]ManagedWebhook, error) {
	hook := &gh.Hook{
		Config: &gh.HookConfig{
			URL:         gh.Ptr(webhookURL),
			ContentType: gh.Ptr("json"),
			Secret:      gh.Ptr(secret),
			InsecureSSL: gh.Ptr("0"),
		},
		Events: []string{"workflow_job"},
		Active: gh.Ptr(true),
	}

	if c.IsOrgLevel() {
		if id, ok := c.findOrgHook(ctx, webhookURL); ok {
			c.cfg.Log.Info("org webhook already registered, skipping create", "hook_id", id, "url", webhookURL)
			return []ManagedWebhook{{HookID: id}}, nil
		}
		created, _, err := c.client.Organizations.CreateHook(ctx, c.cfg.Owner, hook)
		if err != nil {
			return nil, fmt.Errorf("creating org webhook for %s: %w", c.cfg.Owner, err)
		}
		c.cfg.Log.Info("registered org webhook", "hook_id", created.GetID(), "url", webhookURL)
		return []ManagedWebhook{{HookID: created.GetID()}}, nil
	}

	var managed []ManagedWebhook
	for _, repo := range c.cfg.Repos {
		if id, ok := c.findRepoHook(ctx, repo, webhookURL); ok {
			c.cfg.Log.Info("repo webhook already registered, skipping create", "repo", repo, "hook_id", id, "url", webhookURL)
			managed = append(managed, ManagedWebhook{Repo: repo, HookID: id})
			continue
		}
		created, _, err := c.client.Repositories.CreateHook(ctx, c.cfg.Owner, repo, hook)
		if err != nil {
			// Clean up any hooks we already created
			for _, m := range managed {
				if delErr := c.deleteWebhook(ctx, m); delErr != nil {
					c.cfg.Log.Warn("failed to clean up webhook after partial failure", "repo", m.Repo, "error", delErr)
				}
			}
			return nil, fmt.Errorf("creating webhook for %s/%s: %w", c.cfg.Owner, repo, err)
		}
		c.cfg.Log.Info("registered repo webhook", "repo", repo, "hook_id", created.GetID(), "url", webhookURL)
		managed = append(managed, ManagedWebhook{Repo: repo, HookID: created.GetID()})
	}

	return managed, nil
}

// findOrgHook returns the ID of an existing org-level hook whose config URL
// matches webhookURL. The bool is false when none matches or the list call
// fails — callers then fall back to CreateHook (surfacing any real error, e.g.
// a genuine 422, at create time rather than swallowing it here).
func (c *Client) findOrgHook(ctx context.Context, webhookURL string) (int64, bool) {
	hooks, _, err := c.client.Organizations.ListHooks(ctx, c.cfg.Owner, nil)
	if err != nil {
		c.cfg.Log.Debug("could not list org webhooks for idempotency check", "error", err)
		return 0, false
	}
	for _, h := range hooks {
		if hookURLMatches(h, webhookURL) {
			return h.GetID(), true
		}
	}
	return 0, false
}

// findRepoHook returns the ID of an existing repo hook whose config URL matches
// webhookURL. See findOrgHook for the false semantics.
func (c *Client) findRepoHook(ctx context.Context, repo, webhookURL string) (int64, bool) {
	hooks, _, err := c.client.Repositories.ListHooks(ctx, c.cfg.Owner, repo, nil)
	if err != nil {
		c.cfg.Log.Debug("could not list repo webhooks for idempotency check", "repo", repo, "error", err)
		return 0, false
	}
	for _, h := range hooks {
		if hookURLMatches(h, webhookURL) {
			return h.GetID(), true
		}
	}
	return 0, false
}

// hookURLMatches reports whether a hook's config URL equals webhookURL.
func hookURLMatches(h *gh.Hook, webhookURL string) bool {
	if h == nil || h.Config == nil {
		return false
	}
	return h.Config.GetURL() == webhookURL
}

// DeregisterWebhooks removes all managed webhooks. Called on shutdown.
func (c *Client) DeregisterWebhooks(ctx context.Context, hooks []ManagedWebhook) {
	for _, m := range hooks {
		if err := c.deleteWebhook(ctx, m); err != nil {
			c.cfg.Log.Warn("failed to remove webhook on shutdown", "repo", m.Repo, "hook_id", m.HookID, "error", err)
		} else {
			if m.Repo == "" {
				c.cfg.Log.Info("removed org webhook", "hook_id", m.HookID)
			} else {
				c.cfg.Log.Info("removed repo webhook", "repo", m.Repo, "hook_id", m.HookID)
			}
		}
	}
}

// CleanStaleWebhooks removes any workflow_job webhooks left behind by previous
// ephemerd instances that crashed or were killed without cleanup. Called on
// startup before registering new webhooks to avoid hitting GitHub's 20-hook limit.
func (c *Client) CleanStaleWebhooks(ctx context.Context) {
	if c.IsOrgLevel() {
		hooks, _, err := c.client.Organizations.ListHooks(ctx, c.cfg.Owner, nil)
		if err != nil {
			c.cfg.Log.Debug("could not list org webhooks for cleanup", "error", err)
			return
		}
		for _, h := range hooks {
			if hasEvent(h.Events, "workflow_job") {
				if _, err := c.client.Organizations.DeleteHook(ctx, c.cfg.Owner, h.GetID()); err != nil {
					c.cfg.Log.Warn("failed to remove stale org webhook", "hook_id", h.GetID(), "error", err)
				} else {
					c.cfg.Log.Info("removed stale org webhook", "hook_id", h.GetID(), "url", h.GetURL())
				}
			}
		}
		return
	}

	for _, repo := range c.cfg.Repos {
		hooks, _, err := c.client.Repositories.ListHooks(ctx, c.cfg.Owner, repo, nil)
		if err != nil {
			c.cfg.Log.Debug("could not list repo webhooks for cleanup", "repo", repo, "error", err)
			continue
		}
		for _, h := range hooks {
			if hasEvent(h.Events, "workflow_job") {
				if _, err := c.client.Repositories.DeleteHook(ctx, c.cfg.Owner, repo, h.GetID()); err != nil {
					c.cfg.Log.Warn("failed to remove stale webhook", "repo", repo, "hook_id", h.GetID(), "error", err)
				} else {
					c.cfg.Log.Info("removed stale webhook", "repo", repo, "hook_id", h.GetID())
				}
			}
		}
	}
}

func hasEvent(events []string, target string) bool {
	for _, e := range events {
		if e == target {
			return true
		}
	}
	return false
}

// PingWebhook triggers a ping event for a managed webhook.
func (c *Client) PingWebhook(ctx context.Context, m ManagedWebhook) error {
	if m.Repo == "" {
		_, err := c.client.Organizations.PingHook(ctx, c.cfg.Owner, m.HookID)
		return err
	}
	_, err := c.client.Repositories.PingHook(ctx, c.cfg.Owner, m.Repo, m.HookID)
	return err
}

func (c *Client) deleteWebhook(ctx context.Context, m ManagedWebhook) error {
	if m.Repo == "" {
		_, err := c.client.Organizations.DeleteHook(ctx, c.cfg.Owner, m.HookID)
		return err
	}
	_, err := c.client.Repositories.DeleteHook(ctx, c.cfg.Owner, m.Repo, m.HookID)
	return err
}
