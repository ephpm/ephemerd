// Package github implements providers.Provider for GitHub Actions.
//
// This is a thin adapter around the existing pkg/github.Client,
// translating its types into the provider-neutral interface.
package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ephpm/ephemerd/pkg/github"
	"github.com/ephpm/ephemerd/pkg/providers"
)

const defaultImage = "ghcr.io/actions/actions-runner:latest"

// Provider implements providers.Poll and providers.Webhook for GitHub Actions.
type Provider struct {
	client       *github.Client
	log          *slog.Logger
	events       chan providers.JobEvent
	webhooks     []github.ManagedWebhook
	whHandler    http.Handler
	whEvents     <-chan providers.JobEvent
	cancel       context.CancelFunc
	defaultImage string // config override for the runner container image
}

// Compile-time interface checks.
var (
	_ providers.Poll    = (*Provider)(nil)
	_ providers.Webhook = (*Provider)(nil)
)

// New creates a GitHub provider wrapping an existing GitHub client.
// imageOverride, if non-empty, replaces the default runner container image.
func New(client *github.Client, log *slog.Logger, imageOverride string) *Provider {
	return &Provider{
		client:       client,
		log:          log,
		events:       make(chan providers.JobEvent, 64),
		defaultImage: imageOverride,
	}
}

func (p *Provider) Name() string { return "github" }
func (p *Provider) DefaultImage() string {
	if p.defaultImage != "" {
		return p.defaultImage
	}
	return defaultImage
}
func (p *Provider) DefaultJobImage() string { return "" }

func (p *Provider) Start(ctx context.Context, cfg providers.PollConfig) (<-chan providers.JobEvent, error) {
	ctx, p.cancel = context.WithCancel(ctx)

	interval := time.Duration(cfg.PollInterval) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	go p.pollLoop(ctx, interval)

	return p.events, nil
}

func (p *Provider) pollLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			events, err := p.client.PollJobs(ctx)
			if err != nil {
				p.log.Debug("poll failed", "error", err)
				continue
			}
			for _, ev := range events {
				select {
				case p.events <- p.convertEvent(ev):
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (p *Provider) ClaimJob(ctx context.Context, event *providers.JobEvent, runnerName string, labels []string) (*providers.Claim, error) {
	jitConfig, err := p.client.RegisterJITRunner(ctx, event.Repo, runnerName, labels)
	if err != nil {
		return nil, fmt.Errorf("registering JIT runner: %w", err)
	}

	return &providers.Claim{
		RunnerID:     jitConfig.GetRunner().GetID(),
		RunnerName:   runnerName,
		Repo:         event.Repo,
		RunnerConfig: jitConfig.GetEncodedJITConfig(),
	}, nil
}

func (p *Provider) ReleaseJob(ctx context.Context, claim *providers.Claim) error {
	return p.client.RemoveRunner(ctx, claim.Repo, claim.RunnerID)
}

func (p *Provider) FetchJobImage(ctx context.Context, event *providers.JobEvent) string {
	return p.client.FetchJobImage(ctx, event.Repo, event.RunID, event.JobID)
}

func (p *Provider) WebhookHandler(secret string) (http.Handler, <-chan providers.JobEvent) {
	handler, ghCh := p.client.WebhookHandler(secret)
	p.whHandler = handler

	// Convert GitHub events to provider events on the fly.
	ch := make(chan providers.JobEvent, 64)
	go func() {
		for ev := range ghCh {
			ch <- p.convertEvent(ev)
		}
		close(ch)
	}()
	p.whEvents = ch

	return handler, ch
}

func (p *Provider) RegisterWebhooks(ctx context.Context, url, secret string) error {
	hooks, err := p.client.RegisterWebhooks(ctx, url, secret)
	if err != nil {
		return fmt.Errorf("registering github webhooks: %w", err)
	}
	p.webhooks = hooks
	return nil
}

func (p *Provider) DeregisterWebhooks(ctx context.Context) error {
	p.client.DeregisterWebhooks(ctx, p.webhooks)
	p.webhooks = nil
	return nil
}

// CleanStaleWebhooks removes any workflow_job webhooks left behind by previous
// ephemerd instances that crashed or were killed without cleanup. Called on
// startup before registering new webhooks to avoid hitting GitHub's 20-hook limit.
func (p *Provider) CleanStaleWebhooks(ctx context.Context) {
	p.client.CleanStaleWebhooks(ctx)
}

// CatchUpPoll fires a single poll to discover jobs queued while ephemerd was
// offline. Used in webhook mode (where continuous polling is disabled) to catch
// jobs that transitioned to "queued" before webhooks could be registered —
// webhook events aren't replayed for jobs already in that state.
func (p *Provider) CatchUpPoll(ctx context.Context) error {
	events, err := p.client.PollJobs(ctx)
	if err != nil {
		return fmt.Errorf("startup poll: %w", err)
	}
	for _, ev := range events {
		select {
		case p.events <- p.convertEvent(ev):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (p *Provider) Stop(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	if len(p.webhooks) > 0 {
		p.client.DeregisterWebhooks(ctx, p.webhooks)
	}
	return nil
}

func (p *Provider) convertEvent(ev github.JobEvent) providers.JobEvent {
	var labels []string
	if ev.Job != nil {
		labels = append(labels, ev.Job.Labels...)
	}

	fe := providers.JobEvent{
		Provider: p,
		Action:   ev.Action,
		Repo:     ev.Repo,
		Labels:   labels,
		Raw:      ev.Job,
	}
	if ev.Job != nil {
		fe.JobID = ev.Job.GetID()
		fe.RunID = ev.Job.GetRunID()
		fe.Conclusion = ev.Job.GetConclusion()
	}
	return fe
}
