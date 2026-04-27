// Package woodpecker implements providers.Provider for Woodpecker CI.
//
// Woodpecker CI (community fork of Drone) uses agents that connect to the
// Woodpecker server via gRPC. The server dispatches pipelines defined in
// .woodpecker.yml to connected agents. Agents execute pipeline steps in
// Docker containers.
//
// Integration model:
//
//	ephemerd maintains a pool of Woodpecker agent containers. Each agent
//	connects to the Woodpecker server using a shared secret, receives
//	pipelines, and executes them. When an agent finishes a pipeline,
//	ephemerd replaces it — same pool model as Forgejo/Gitea.
//
//	The Woodpecker server itself requires a forge backend (Gitea, Forgejo,
//	GitHub, or GitLab) for repo management and webhook-driven triggers.
//	ephemerd does not manage the server — only the agents.
//
// Reference:
//   - Server docs: https://woodpecker-ci.org/docs/administration/server-config
//   - Agent docs:  https://woodpecker-ci.org/docs/administration/agent-config
//   - API docs:    https://woodpecker-ci.org/docs/usage/api
package woodpecker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ephpm/ephemerd/pkg/providers"
)

const defaultImage = "docker.io/woodpeckerci/woodpecker-agent:latest"

// Compile-time interface check.
var _ providers.Poll = (*Provider)(nil)

// Config for the Woodpecker provider.
type Config struct {
	// ServerURL is the gRPC address of the Woodpecker server
	// (e.g., "woodpecker.example.com:9000").
	ServerURL string

	// AgentSecret is the shared secret between server and agent.
	// Set via WOODPECKER_AGENT_SECRET on the server side.
	AgentSecret string

	Log *slog.Logger
}

// Provider implements providers.Provider for Woodpecker CI.
type Provider struct {
	cfg    Config
	events chan providers.JobEvent
	cancel context.CancelFunc
}

// New creates a Woodpecker provider.
func New(cfg Config) (*Provider, error) {
	if cfg.ServerURL == "" {
		return nil, fmt.Errorf("woodpecker: server_url is required")
	}
	if cfg.AgentSecret == "" {
		return nil, fmt.Errorf("woodpecker: agent_secret is required")
	}
	return &Provider{
		cfg:    cfg,
		events: make(chan providers.JobEvent, 64),
	}, nil
}

func (p *Provider) Name() string         { return "woodpecker" }
func (p *Provider) DefaultImage() string { return p.DefaultImageFor("linux") }

// DefaultImageFor returns the agent image for the given job OS.
// Woodpecker's agent is Linux-only upstream; Windows returns empty so the
// runtime can pick its host fallback if a workflow ever targets Windows.
func (p *Provider) DefaultImageFor(os string) string {
	if os == "linux" {
		return defaultImage
	}
	return ""
}

// DefaultJobImage returns empty — the Woodpecker agent creates job containers
// based on the image: field in .woodpecker.yml pipeline steps.
func (p *Provider) DefaultJobImage() string { return "" }

func (p *Provider) Start(ctx context.Context, cfg providers.PollConfig) (<-chan providers.JobEvent, error) {
	ctx, p.cancel = context.WithCancel(ctx)

	// Woodpecker agents connect to the server via gRPC and receive
	// pipelines directly. ephemerd doesn't poll — the agent handles it.
	// Start() sets up the agent pool lifecycle.
	//
	// TODO: implement agent pool management
	// - Spin up N agent containers (N = max_concurrent)
	// - Each agent connects to ServerURL with AgentSecret
	// - When an agent container exits, replace it
	//
	// For now, emit events when the Woodpecker server API shows
	// pending pipelines (poll-based discovery as a fallback).
	go p.pollLoop(ctx, cfg.PollInterval)

	p.cfg.Log.Info("woodpecker agent pool started",
		"server", p.cfg.ServerURL,
	)

	return p.events, nil
}

func (p *Provider) pollLoop(ctx context.Context, intervalSec int) {
	// TODO: poll Woodpecker server API for pipeline status
	//
	// GET /api/pipelines?status=pending
	//
	// The Woodpecker REST API requires a bearer token.
	// For the pool model, polling is optional — agents handle discovery.
}

func (p *Provider) ClaimJob(ctx context.Context, event *providers.JobEvent, runnerName string, labels []string) (*providers.Claim, error) {
	// Woodpecker agents self-register via gRPC shared secret.
	// No per-job registration needed — the agent picks up work automatically.
	return &providers.Claim{
		RunnerName: runnerName,
		Repo:       event.Repo,
		Env: map[string]string{
			"WOODPECKER_SERVER":       p.cfg.ServerURL,
			"WOODPECKER_AGENT_SECRET": p.cfg.AgentSecret,
		},
	}, nil
}

func (p *Provider) ReleaseJob(ctx context.Context, claim *providers.Claim) error {
	// Agent handles pipeline completion reporting.
	return nil
}

func (p *Provider) FetchJobImage(ctx context.Context, event *providers.JobEvent) string {
	// Pipeline image is defined in .woodpecker.yml — the agent resolves it.
	return ""
}

func (p *Provider) Stop(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}
