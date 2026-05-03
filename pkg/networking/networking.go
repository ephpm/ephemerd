package networking

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
)

// DefaultSubnet is the preferred IP range for containers.
const DefaultSubnet = "10.88.0.0/16"

// Config for container networking.
type Config struct {
	DataDir       string
	Subnet        string // container subnet (auto-selected if empty)
	MTU           int    // bridge MTU (auto-detected from host if 0)
	CNIBinDir     string // path to CNI plugin binaries (Linux only, ignored elsewhere)
	GatewayPorts  []int  // extra TCP ports to allow from containers to the gateway (e.g., module proxy)
	Log           *slog.Logger
}

// pickSubnet tries the default subnet first. If it conflicts with an existing
// interface, picks a random 10.x.0.0/16 subnet that's free.
func pickSubnet(log *slog.Logger) string {
	return pickSubnetFromAddrs(log, hostInterfaceAddrs())
}

// pickSubnetFromAddrs is the testable core of pickSubnet — given a snapshot
// of interface addresses, picks a non-conflicting subnet using the same
// strategy: prefer DefaultSubnet, retry up to 10 random 10.x.0.0/16 ranges,
// then fall back to 10.199.0.0/16. Extracted so unit tests can feed in
// fakes without touching the host's real network configuration.
func pickSubnetFromAddrs(log *slog.Logger, addrs []net.Addr) string {
	if !subnetInUseAmong(DefaultSubnet, addrs) {
		return DefaultSubnet
	}
	log.Info("default subnet in use, picking alternative", "subnet", DefaultSubnet)

	for range 10 {
		second := rand.IntN(256)
		candidate := fmt.Sprintf("10.%d.0.0/16", second)
		if !subnetInUseAmong(candidate, addrs) {
			log.Info("selected subnet", "subnet", candidate)
			return candidate
		}
	}

	// Give up and use a high range unlikely to conflict
	return "10.199.0.0/16"
}

// hostInterfaceAddrs gathers all addresses from all host interfaces.
// Errors are swallowed (returning a partial or empty list) because the
// caller — pickSubnet — already has a "give up and use 10.199.0.0/16"
// fallback for the no-information case.
func hostInterfaceAddrs() []net.Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Addr
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		out = append(out, addrs...)
	}
	return out
}

// subnetInUse checks if any network interface has an address in the given CIDR.
func subnetInUse(cidr string) bool {
	return subnetInUseAmong(cidr, hostInterfaceAddrs())
}

// subnetInUseAmong reports whether any of the given addresses fall inside cidr.
// Returns false for malformed CIDRs and for addresses that fail to parse.
func subnetInUseAmong(cidr string, addrs []net.Addr) bool {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			continue
		}
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
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
	// EndpointID is the HCN endpoint ID (Windows only). Used to attach
	// the network to the container via the OCI spec.
	EndpointID string
	// IP is the container's IP address on the bridge network.
	// Populated on Linux from the CNI result; empty on other platforms.
	IP string
}

// platformNetworking is implemented per-OS.
type platformNetworking interface {
	init(cfg Config) error
	setup(ctx context.Context, id string, netns string) (*SetupResult, error)
	teardown(ctx context.Context, id string, netns string) error
	installFirewallRules() error
	removeFirewallRules()
	cleanup()
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

// GatewayIP returns the bridge gateway IP address (e.g., "10.88.0.1").
// This is the first usable IP in the container subnet, reachable from
// inside containers. Used by services that need to be accessible to jobs
// (e.g., Go module proxy, DNS).
func (m *Manager) GatewayIP() string {
	subnet := m.cfg.Subnet
	if subnet == "" {
		subnet = DefaultSubnet
	}
	ip, _, err := net.ParseCIDR(subnet)
	if err != nil {
		return "10.88.0.1"
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return "10.88.0.1"
	}
	ip4[3] = 1
	return ip4.String()
}

// InstallFirewallRules blocks container traffic to private network ranges.
func (m *Manager) InstallFirewallRules() error {
	return m.platform.installFirewallRules()
}

// Cleanup removes all networking state: firewall rules, bridge interface,
// CNI config, IP allocations, and DNS files. Called on shutdown.
func (m *Manager) Cleanup() {
	m.platform.removeFirewallRules()
	m.platform.cleanup()
}

// CleanStaleBridge deletes the ephemerd0 bridge if it exists. Used on startup
// in the WSL containerd-only worker to remove bridges left over from a previous
// boot (all WSL2 distros share one kernel so bridges persist across instances).
func CleanStaleBridge(log *slog.Logger) {
	cleanStaleBridge(log)
}
