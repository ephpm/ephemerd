//go:build windows

package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Microsoft/hcsshim/hcn"
)

const (
	networkName    = "ephemerd"
	defaultGateway = "10.88.0.1"
)

// windowsEgressNotEnforced is the one-line pointer emitted with the startup and
// per-container WARNs so the reason egress is not contained in software is
// discoverable from the logs alone. The full metal-verified analysis (which
// mechanisms were tried and why each failed on WinNAT + Hyper-V isolation) lives
// in the setup() comment and in the fix/windows-egress-enforce PR.
const windowsEgressNotEnforced = "WinNAT+Hyper-V-isolated job containers have no enforceable per-container egress filter (VFP not enforcing on the NAT switch; host firewall does not filter forwarded/NATed traffic; Hyper-V firewall registers no VMCreator); contain egress at an upstream VLAN"

type windowsNetworking struct {
	cfg     Config
	network *hcn.HostComputeNetwork
	mu      sync.Mutex
}

func newPlatformNetworking() platformNetworking {
	return &windowsNetworking{}
}

func (w *windowsNetworking) init(cfg Config) error {
	w.cfg = cfg

	// Check if network already exists (from previous run)
	existing, err := hcn.GetNetworkByName(networkName)
	if err == nil {
		w.network = existing
		cfg.Log.Info("HCN NAT network found", "name", networkName, "id", existing.Id)
		return nil
	}

	// Create NAT network
	network := &hcn.HostComputeNetwork{
		Name: networkName,
		Type: hcn.NAT,
		Ipams: []hcn.Ipam{
			{
				Type: "Static",
				Subnets: []hcn.Subnet{
					{
						IpAddressPrefix: DefaultSubnet,
						Routes: []hcn.Route{
							{
								NextHop:           defaultGateway,
								DestinationPrefix: "0.0.0.0/0",
							},
						},
					},
				},
			},
		},
		Dns: hcn.Dns{
			ServerList: []string{"8.8.8.8", "8.8.4.4"},
		},
		SchemaVersion: hcn.SchemaVersion{
			Major: 2,
			Minor: 0,
		},
	}

	created, err := network.Create()
	if err != nil {
		return fmt.Errorf("creating HCN NAT network: %w", err)
	}
	w.network = created

	cfg.Log.Info("HCN NAT network created", "name", networkName, "id", created.Id)
	return nil
}

func (w *windowsNetworking) setup(ctx context.Context, id string, netns string) (*SetupResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Create endpoint on the network
	endpoint := &hcn.HostComputeEndpoint{
		Name:               id + "-ep",
		HostComputeNetwork: w.network.Id,
		Dns: hcn.Dns{
			ServerList: []string{"8.8.8.8", "8.8.4.4"},
		},
		SchemaVersion: hcn.SchemaVersion{
			Major: 2,
			Minor: 0,
		},
	}

	created, err := w.network.CreateEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("creating HCN endpoint for %s: %w", id, err)
	}

	// Apply the RFC1918 block ACLs to the endpoint. Fail CLOSED on an *apply*
	// error: if HNS rejects the policy request we tear the endpoint down rather
	// than proceed. The apply succeeding, however, does NOT mean egress is
	// enforced — see the WARN below and windowsEgressNotEnforced.
	if err := w.applyACLPolicies(created); err != nil {
		if delErr := created.Delete(); delErr != nil {
			w.cfg.Log.Warn("failed to delete endpoint after ACL failure", "id", id, "error", delErr)
		}
		return nil, fmt.Errorf("applying egress ACL policies for %s: %w", id, err)
	}

	// HARD TRUTH, verified on metal (Windows Server 2025, build 26100, HNS NAT
	// network, Hyper-V-isolated job containers): the ACLs applied above are
	// STORED on the endpoint but NOT ENFORCED. A live-endpoint inspection
	// confirmed all four Block/Out/Switch ACLs present while the containment
	// suite still reached every management plane (Proxmox, Incus, Grafana).
	//
	// Root cause: on a WinNAT `nat` network the VFP switch-extension datapath is
	// not enforcing on the switch ports (every vfpctrl port/NAT operation fails
	// with "cannot find the file specified"), so RuleType=Switch (VFP) ACLs are
	// inert. And container→LAN egress is *forwarded+SNATed* by the host, a path
	// the host Windows Firewall does not filter (it governs host-terminated
	// traffic, not routing) — which is also why the #136 netsh rules and the
	// #140 Hyper-V-firewall rules (Get-NetFirewallHyperVVMCreator /
	// Get-NetFirewallHyperVPort are empty even with a live container) never bit.
	//
	// There is therefore NO per-container software egress filter available on
	// this host class. Real containment must come from OUTSIDE the box — put the
	// Windows runner's container-egress path on an isolated VLAN whose upstream
	// router denies RFC1918. This WARN exists so the daemon never silently
	// implies a containment it does not provide. See windowsEgressNotEnforced.
	w.cfg.Log.Warn("windows container egress is NOT enforced on this host class "+
		"(WinNAT + Hyper-V isolation): RFC1918 block ACLs are applied but inert; "+
		"containment depends on an upstream/VLAN control, not ephemerd",
		"id", id, "endpoint", created.Id, "detail", windowsEgressNotEnforced)

	// Create an HCN network namespace and attach the endpoint.
	// Hyper-V isolated containers (runhcs) require a pre-existing namespace
	// with the endpoint attached; just putting the endpoint in EndpointList
	// is not sufficient.
	ns := &hcn.HostComputeNamespace{}
	ns, err = ns.Create()
	if err != nil {
		_ = created.Delete()
		return nil, fmt.Errorf("creating HCN namespace for %s: %w", id, err)
	}

	if err := hcn.AddNamespaceEndpoint(ns.Id, created.Id); err != nil {
		_ = ns.Delete()
		_ = created.Delete()
		return nil, fmt.Errorf("attaching endpoint to namespace for %s: %w", id, err)
	}

	w.cfg.Log.Debug("HCN endpoint created", "id", id, "endpoint", created.Id, "namespace", ns.Id)
	return &SetupResult{NetNS: ns.Id, EndpointID: created.Id}, nil
}

func (w *windowsNetworking) teardown(ctx context.Context, id string, netns string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Find endpoint by name
	endpoint, err := hcn.GetEndpointByName(id + "-ep")
	if err != nil {
		return fmt.Errorf("finding HCN endpoint for %s: %w", id, err)
	}

	if netns != "" {
		// Detach endpoint from namespace, then delete the namespace
		_ = endpoint.NamespaceDetach(netns)
		if ns, nsErr := hcn.GetNamespaceByID(netns); nsErr == nil {
			_ = ns.Delete()
		}
	}

	if err := endpoint.Delete(); err != nil {
		return fmt.Errorf("deleting HCN endpoint for %s: %w", id, err)
	}

	w.cfg.Log.Debug("HCN endpoint removed", "id", id)
	return nil
}

// egressBlockedCIDRs are the RFC 1918 + link-local ranges a job container must
// not reach. 169.254.0.0/16 also covers cloud-metadata endpoints
// (169.254.169.254). This is the complete intended egress deny list; every
// entry (except the container's own DefaultSubnet) must become an enforced
// block rule or the container is under-firewalled.
var egressBlockedCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
}

// buildEgressBlockPolicies constructs the per-endpoint block ACLs from
// egressBlockedCIDRs. It fails closed: a marshal error on any rule, or an empty
// resulting set, is an error rather than a silently weaker rule set. Split out
// from applyACLPolicies so the (pure) rule construction is unit-testable
// without a live HCN endpoint.
func buildEgressBlockPolicies() ([]hcn.EndpointPolicy, error) {
	var policies []hcn.EndpointPolicy

	for _, cidr := range egressBlockedCIDRs {
		if cidr == DefaultSubnet {
			continue
		}

		acl := hcn.AclPolicySetting{
			Protocols:       "6,17", // TCP + UDP
			Action:          hcn.ActionTypeBlock,
			Direction:       hcn.DirectionTypeOut,
			RemoteAddresses: cidr,
			RuleType:        hcn.RuleTypeSwitch,
			Priority:        100,
		}

		settings, err := json.Marshal(acl)
		if err != nil {
			// Fail closed: a rule we cannot serialize is a rule we cannot
			// enforce. Do not skip it and continue with a weaker rule set.
			return nil, fmt.Errorf("marshaling egress block ACL for %s: %w", cidr, err)
		}

		policies = append(policies, hcn.EndpointPolicy{
			Type:     hcn.ACL,
			Settings: settings,
		})
	}

	if len(policies) == 0 {
		// Nothing to block would mean no egress restriction at all — treat as
		// an error so the caller refuses to start an unfirewalled container.
		return nil, fmt.Errorf("no egress block ACLs constructed (would run unfirewalled)")
	}

	return policies, nil
}

// applyACLPolicies blocks container traffic to RFC 1918 and link-local ranges.
// The full rule set is built up front and applied atomically; any failure is
// returned so the caller (setup) can treat it as fatal for the job.
func (w *windowsNetworking) applyACLPolicies(endpoint *hcn.HostComputeEndpoint) error {
	policies, err := buildEgressBlockPolicies()
	if err != nil {
		return err
	}

	return endpoint.ApplyPolicy(hcn.RequestTypeAdd, hcn.PolicyEndpointRequest{
		Policies: policies,
	})
}

// installFirewallRules and removeFirewallRules live in firewall_windows.go
// (mirroring firewall_linux.go): the host-global Windows Firewall backstop
// that complements the per-endpoint ACLs applied above.

func (w *windowsNetworking) cleanup() {}

func cleanStaleBridge(_ *slog.Logger) {} // no-op on Windows (HCN, not CNI bridge)
