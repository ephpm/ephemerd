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

	// Apply ACL policies to block private network access. This is the ONLY
	// egress restriction on Windows (there is no global firewall backstop —
	// installFirewallRules is a no-op), so a failure here means the container
	// would otherwise run with unrestricted egress to the host LAN, other
	// RFC1918 services, and link-local metadata endpoints. Fail CLOSED: tear
	// down the endpoint we just created and refuse the job rather than start a
	// container we cannot firewall.
	if err := w.applyACLPolicies(created); err != nil {
		if delErr := created.Delete(); delErr != nil {
			w.cfg.Log.Warn("failed to delete endpoint after ACL failure", "id", id, "error", delErr)
		}
		return nil, fmt.Errorf("applying egress ACL policies for %s (refusing to start unfirewalled): %w", id, err)
	}

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

func (w *windowsNetworking) installFirewallRules() error {
	// ACL policies are applied per-endpoint in setup(), not globally
	w.cfg.Log.Info("Windows ACL firewall policies configured per-endpoint")
	return nil
}

func (w *windowsNetworking) removeFirewallRules() {
	// ACL policies are removed when endpoints are deleted
}

func (w *windowsNetworking) cleanup() {}

func cleanStaleBridge(_ *slog.Logger) {} // no-op on Windows (HCN, not CNI bridge)
