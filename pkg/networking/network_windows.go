//go:build windows

package networking

import (
	"context"
	"encoding/json"
	"fmt"
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

	// Apply ACL policies to block private network access
	if err := w.applyACLPolicies(created); err != nil {
		w.cfg.Log.Warn("failed to apply ACL policies", "id", id, "error", err)
	}

	// Create an HCN network namespace and attach the endpoint.
	// Hyper-V isolated containers (runhcs) require a pre-existing namespace
	// with the endpoint attached; just putting the endpoint in EndpointList
	// is not sufficient.
	ns := &hcn.HostComputeNamespace{}
	ns, err = ns.Create()
	if err != nil {
		created.Delete()
		return nil, fmt.Errorf("creating HCN namespace for %s: %w", id, err)
	}

	if err := hcn.AddNamespaceEndpoint(ns.Id, created.Id); err != nil {
		ns.Delete()
		created.Delete()
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
		endpoint.NamespaceDetach(netns)
		if ns, nsErr := hcn.GetNamespaceByID(netns); nsErr == nil {
			ns.Delete()
		}
	}

	if err := endpoint.Delete(); err != nil {
		return fmt.Errorf("deleting HCN endpoint for %s: %w", id, err)
	}

	w.cfg.Log.Debug("HCN endpoint removed", "id", id)
	return nil
}

// applyACLPolicies blocks container traffic to RFC 1918 and link-local ranges.
func (w *windowsNetworking) applyACLPolicies(endpoint *hcn.HostComputeEndpoint) error {
	blocked := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
	}

	var policies []hcn.EndpointPolicy

	for _, cidr := range blocked {
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
			continue
		}

		policies = append(policies, hcn.EndpointPolicy{
			Type:     hcn.ACL,
			Settings: settings,
		})
	}

	if len(policies) > 0 {
		return endpoint.ApplyPolicy(hcn.RequestTypeAdd, hcn.PolicyEndpointRequest{
			Policies: policies,
		})
	}

	return nil
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
