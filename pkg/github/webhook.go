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
		created, _, err := c.client.Organizations.CreateHook(ctx, c.cfg.Owner, hook)
		if err != nil {
			return nil, fmt.Errorf("creating org webhook for %s: %w", c.cfg.Owner, err)
		}
		c.cfg.Log.Info("registered org webhook", "hook_id", created.GetID(), "url", webhookURL)
		return []ManagedWebhook{{HookID: created.GetID()}}, nil
	}

	var managed []ManagedWebhook
	for _, repo := range c.cfg.Repos {
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
