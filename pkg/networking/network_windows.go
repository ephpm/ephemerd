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

	// l2BridgeNetworkName is the HNS network created on the L2Bridge egress
	// path. It is deliberately distinct from networkName so a host that
	// previously ran the NAT path (network "ephemerd") does not collide with,
	// or get mistaken for, the L2Bridge network.
	l2BridgeNetworkName = "ephemerd-l2bridge"
)

// defaultPublicDNS is the DNS resolver list handed to L2Bridge containers when
// Config.PublicDNS is empty. Public resolvers so container DNS never needs the
// LAN router — which the egress ACLs block along with the rest of RFC1918.
var defaultPublicDNS = []string{"1.1.1.1", "8.8.8.8"}

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

	// L2Bridge egress path (opt-in). NAT stays the default: only a pool that
	// explicitly sets L2BridgeEgress reaches VFP-enforced egress filtering.
	if cfg.L2BridgeEgress {
		return w.initL2Bridge(cfg)
	}

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

// publicDNS returns the configured public resolver list, or the built-in
// default when none is configured.
func (w *windowsNetworking) publicDNS() []string {
	if len(w.cfg.PublicDNS) > 0 {
		return w.cfg.PublicDNS
	}
	return defaultPublicDNS
}

// initL2Bridge creates (or adopts) the L2Bridge HNS network bound to the
// configured host NIC. Unlike the NAT network, this puts containers on a
// VFP-managed vSwitch port so the per-endpoint Switch ACLs applied in setup()
// actually enforce.
//
// DNS is overridden to the public resolvers at the network and endpoint level
// so the container never needs the LAN router for name resolution — the egress
// ACLs block the router.
//
// UNRESOLVED — IPAM. This declares NO Ipams, intending the container's vNIC to
// DHCP on the LAN. HNS does not support that: on metal (Server 2025 26100)
// Create() fails outright with
//
//	hcnCreateNetwork failed in Win32: The network does not have a subnet for
//	this endpoint. (0x803b0005) / ErrorCode 2151350277
//
// so no L2Bridge network, endpoint, or container can be created at all while
// this path is taken. An L2Bridge network requires an Ipam subnet with a
// default route; with one present HNS assigns endpoint addresses itself (no
// IpConfigurations needed) and the rest of this path — endpoint creation,
// namespace attach, and the egress ACL ladder — was verified working on metal.
//
// Adopting that means ephemerd hands containers addresses out of the real LAN
// subnet, which needs a range the LAN's DHCP server will not also hand out.
// That allocation decision is why this is not simply switched over here.
func (w *windowsNetworking) initL2Bridge(cfg Config) error {
	if cfg.HostNIC == "" {
		// Fail closed: without an uplink NIC the bridge has no path off-host,
		// and silently falling back to NAT would defeat the egress guarantee
		// the operator opted into.
		return fmt.Errorf("L2Bridge egress enabled but no host_nic configured (set network.host_nic to the host adapter name to bridge onto)")
	}

	if existing, err := hcn.GetNetworkByName(l2BridgeNetworkName); err == nil {
		w.network = existing
		cfg.Log.Info("HCN L2Bridge network found", "name", l2BridgeNetworkName, "id", existing.Id, "host_nic", cfg.HostNIC)
		return nil
	}

	adapterPol, err := json.Marshal(hcn.NetAdapterNameNetworkPolicySetting{
		NetworkAdapterName: cfg.HostNIC,
	})
	if err != nil {
		return fmt.Errorf("marshaling NetAdapterName policy for %q: %w", cfg.HostNIC, err)
	}

	network := &hcn.HostComputeNetwork{
		Name: l2BridgeNetworkName,
		Type: hcn.L2Bridge,
		// No Ipams. See the UNRESOLVED note above: HNS rejects this
		// (0x803b0005) — an L2Bridge network must carry a subnet.
		Policies: []hcn.NetworkPolicy{
			{
				Type:     hcn.NetAdapterName, // binds the L2Bridge to the physical NIC
				Settings: adapterPol,
			},
		},
		Dns: hcn.Dns{
			ServerList: w.publicDNS(),
		},
		SchemaVersion: hcn.SchemaVersion{
			Major: 2,
			Minor: 0,
		},
	}

	created, err := network.Create()
	if err != nil {
		return fmt.Errorf("creating HCN L2Bridge network on %q: %w", cfg.HostNIC, err)
	}
	w.network = created

	cfg.Log.Info("HCN L2Bridge network created", "name", l2BridgeNetworkName, "id", created.Id, "host_nic", cfg.HostNIC)
	return nil
}

func (w *windowsNetworking) setup(ctx context.Context, id string, netns string) (*SetupResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// DNS: on L2Bridge, hand the container the configured PUBLIC resolvers so
	// name resolution goes to the internet (permitted by the low-precedence
	// allow-any ACL) and never to the LAN router (blocked by the RFC1918 ACLs).
	// On NAT, DNS is the gateway's forwarder as before.
	dnsServers := []string{"8.8.8.8", "8.8.4.4"}
	if w.cfg.L2BridgeEgress {
		dnsServers = w.publicDNS()
	}

	// Create endpoint on the network. On the L2Bridge path we deliberately set
	// NO IpConfigurations so the container leases its address, gateway, and
	// default route from the LAN DHCP server (DHCP IPAM). On NAT, HNS assigns
	// from the NAT subnet as before.
	endpoint := &hcn.HostComputeEndpoint{
		Name:               id + "-ep",
		HostComputeNetwork: w.network.Id,
		Dns: hcn.Dns{
			ServerList: dnsServers,
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

	// Apply ACL policies to block private network access. These are the only
	// egress restriction that actually enforces on this path: installFirewallRules
	// is NOT a no-op (firewall_windows.go programs Hyper-V firewall rules), but
	// those rules are built for the NAT subnet — hyperVEgressRules is called with
	// DefaultSubnet/defaultGateway — so on L2Bridge, where the container holds a
	// LAN address, they match nothing. A failure here therefore means the
	// container would run with unrestricted egress to the host LAN, other RFC1918
	// services, and link-local metadata endpoints. Fail CLOSED: tear down the
	// endpoint we just created and refuse the job rather than start a container we
	// cannot firewall.
	//
	// The ACLs are applied to the endpoint BEFORE the container is started
	// (setup runs ahead of task creation), and the L2Bridge rule set is STATIC
	// — it does not depend on the gateway or DNS (DNS is public, the
	// router gets no allow) — so there is no window in which the
	// container has LAN connectivity but no filters.
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

// VFP Switch-ACL precedence for the L2Bridge egress model. VFP evaluates ACLs
// by Priority, LOWER number = HIGHER precedence (evaluated first, first match
// wins). The ladder is: operator carve-outs (top) > RFC1918 block > allow-any
// (bottom). The two allow-any rules at the bottom are what stop the port from
// default-denying everything — VFP is default-DENY the moment any ACL is
// present.
//
// Every rule in this ladder is address-scoped. See the note in
// buildL2BridgeEgressACLPolicies: mixing in a port-scoped rule with no address
// scope (the removed UDP 67/68 DHCP allows) blackholes the entire port on
// metal.
const (
	// aclPriorityExtraAllow carves configured destinations out ABOVE the
	// RFC1918 block. Unused by default (no carve-outs) — reserved for future
	// operator-allowed destinations.
	aclPriorityExtraAllow uint16 = 95
	// aclPriorityBlock denies the RFC1918 + link-local supernets, whole. No
	// gateway/own-subnet carve-out: on L2Bridge the container is a LAN peer,
	// so carving the subnet would expose the management plane and the router.
	aclPriorityBlock uint16 = 100
	// aclPriorityAllowAny permits everything not blocked above (the internet)
	// and, crucially, inbound return traffic. Lowest precedence.
	aclPriorityAllowAny uint16 = 65500
)

// aclAnyProtocol matches any IP protocol in an HNS ACL ("256"). Every rule in
// the ladder uses it, per the proven metal run.
const aclAnyProtocol = "256"

// marshalACL serializes one AclPolicySetting into an EndpointPolicy, failing
// closed on a marshal error (a rule we cannot serialize is a rule we cannot
// enforce; never skip it and continue with a weaker set).
func marshalACL(acl hcn.AclPolicySetting) (hcn.EndpointPolicy, error) {
	settings, err := json.Marshal(acl)
	if err != nil {
		return hcn.EndpointPolicy{}, fmt.Errorf("marshaling ACL %+v: %w", acl, err)
	}
	return hcn.EndpointPolicy{Type: hcn.ACL, Settings: settings}, nil
}

// buildL2BridgeEgressACLPolicies constructs the router-safe VFP Switch-ACL set
// for an L2Bridge endpoint. It is a pure function (no HCN calls) so the exact
// emitted policy set — actions, directions, remotes, protocols, priorities — is
// unit-testable. Fails closed on any marshal error.
//
// The model matches the Linux end-state (firewall_linux.go): block ALL of
// 10/8, 172.16/12, 192.168/16, 169.254/16 — including the LAN router and the
// container's own subnet — and permit only the internet. DNS is handled out of
// band (public resolvers on the endpoint), so unlike Linux there is no DNS or
// gateway carve-out here, and no container-to-container allow (that would be
// LAN access, which we block). extraAllowed carves additional CIDRs out above
// the block for future use; empty (the default) reproduces the strict posture.
func buildL2BridgeEgressACLPolicies(extraAllowed []string) ([]hcn.EndpointPolicy, error) {
	var policies []hcn.EndpointPolicy
	add := func(acl hcn.AclPolicySetting) error {
		p, err := marshalACL(acl)
		if err != nil {
			return err
		}
		policies = append(policies, p)
		return nil
	}

	// NO DHCP tier. An earlier revision allowed UDP 67/68 (Out with
	// RemotePorts, In with LocalPorts, no address scope) at a precedence above
	// the block, so a lease/renew could survive the RFC1918 deny. On metal
	// those two rules BLACKHOLE THE PORT: with them present nothing egresses at
	// all — not the internet, not even a destination carved out by an explicit
	// higher-precedence Allow.
	//
	// Measured on mfl-win-amd64-101 (Server 2025 26100), same endpoint, same
	// code path, only these two rules varying:
	//
	//	blocks + allow-any + DHCP rules -> ALL probes fail (1.1.1.1, 8.8.8.8,
	//	                                   DNS, and every RFC1918 target)
	//	blocks + allow-any              -> exactly the intended posture:
	//	                                   Grafana/Incus/Proxmox/router/host
	//	                                   blocked, 1.1.1.1 + 8.8.8.8 + DNS +
	//	                                   public HTTPS all reachable
	//
	// HNS accepts the rules (ApplyPolicy returns success) but the resulting VFP
	// rule set drops everything, so this fails CLOSED rather than open — it
	// breaks jobs instead of leaking. Port-scoped ACLs carrying no address
	// scope must not be mixed into this ladder.
	//
	// Nothing here needs DHCP: the endpoint is addressed by HNS IPAM, not by a
	// DHCP client in the container.

	// Tier: operator carve-outs (future use). Allowed ABOVE the block so a
	// listed destination wins. Empty by default.
	for _, cidr := range extraAllowed {
		if err := add(hcn.AclPolicySetting{
			Protocols:       aclAnyProtocol,
			Action:          hcn.ActionTypeAllow,
			Direction:       hcn.DirectionTypeOut,
			RemoteAddresses: cidr,
			RuleType:        hcn.RuleTypeSwitch,
			Priority:        aclPriorityExtraAllow,
		}); err != nil {
			return nil, err
		}
	}

	// Tier: block the RFC1918 + link-local supernets, whole. No carve-out.
	for _, cidr := range egressBlockedCIDRs {
		if err := add(hcn.AclPolicySetting{
			Protocols:       aclAnyProtocol,
			Action:          hcn.ActionTypeBlock,
			Direction:       hcn.DirectionTypeOut,
			RemoteAddresses: cidr,
			RuleType:        hcn.RuleTypeSwitch,
			Priority:        aclPriorityBlock,
		}); err != nil {
			return nil, err
		}
	}

	// Tier: allow-any Out AND In (lowest precedence). BOTH are mandatory:
	// without them the port default-denies everything (internet included), and
	// the INBOUND allow is required or TCP return traffic (SYN-ACK) is dropped
	// and even permitted destinations fail.
	for _, dir := range []hcn.DirectionType{hcn.DirectionTypeOut, hcn.DirectionTypeIn} {
		if err := add(hcn.AclPolicySetting{
			Protocols:       aclAnyProtocol,
			Action:          hcn.ActionTypeAllow,
			Direction:       dir,
			RemoteAddresses: "0.0.0.0/0",
			RuleType:        hcn.RuleTypeSwitch,
			Priority:        aclPriorityAllowAny,
		}); err != nil {
			return nil, err
		}
	}

	return policies, nil
}

// applyACLPolicies applies the per-endpoint egress ACLs. On the L2Bridge path
// it applies the router-safe VFP ladder (buildL2BridgeEgressACLPolicies); on
// NAT it applies the existing block-only set (buildEgressBlockPolicies), which
// is left untouched so the default NAT path behaves exactly as before. The full
// rule set is built up front and applied atomically; any failure is returned so
// the caller (setup) can treat it as fatal for the job.
func (w *windowsNetworking) applyACLPolicies(endpoint *hcn.HostComputeEndpoint) error {
	var (
		policies []hcn.EndpointPolicy
		err      error
	)
	if w.cfg.L2BridgeEgress {
		policies, err = buildL2BridgeEgressACLPolicies(w.cfg.ExtraAllowedCIDRs)
	} else {
		policies, err = buildEgressBlockPolicies()
	}
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
