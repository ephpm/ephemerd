package networking

import (
	"context"
	"log/slog"
)

// DefaultSubnet is the IP range for containers.
const DefaultSubnet = "10.88.0.0/16"

// Config for container networking.
type Config struct {
	DataDir string
	Log     *slog.Logger
}

// Manager handles platform-specific container networking.
// On Linux, this uses CNI with a bridge and iptables firewall.
// On Windows, this uses HCN with a NAT network and ACL policies.
type Manager struct {
	cfg      Config
	platform platformNetworking
}

// SetupResult contains the network configuration applied to a container.
type SetupResult struct {
	// NetNS is the network namespace identifier (Linux: path, Windows: namespace ID).
	NetNS string
}

// platformNetworking is implemented per-OS.
type platformNetworking interface {
	init(cfg Config) error
	setup(ctx context.Context, id string, netns string) (*SetupResult, error)
	teardown(ctx context.Context, id string, netns string) error
	installFirewallRules() error
	removeFirewallRules()
}

// New creates and initializes the networking manager for the current platform.
func New(cfg Config) (*Manager, error) {
	m := &Manager{cfg: cfg}

	p := newPlatformNetworking()
	if err := p.init(cfg); err != nil {
		return nil, err
	}
	m.platform = p

	return m, nil
}

// Setup attaches a container to the network.
func (m *Manager) Setup(ctx context.Context, id string, netns string) (*SetupResult, error) {
	return m.platform.setup(ctx, id, netns)
}

// Teardown detaches a container from the network.
func (m *Manager) Teardown(ctx context.Context, id string, netns string) error {
	return m.platform.teardown(ctx, id, netns)
}

// InstallFirewallRules blocks container traffic to private network ranges.
func (m *Manager) InstallFirewallRules() error {
	return m.platform.installFirewallRules()
}

// RemoveFirewallRules cleans up firewall rules on shutdown.
func (m *Manager) RemoveFirewallRules() {
	m.platform.removeFirewallRules()
}
