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

	// plan and ipam are set only on the L2Bridge path: the resolved LAN
	// address plan, and the allocator that hands each endpoint an address out
	// of the operator's reserved pool.
	plan *l2BridgePlan
	ipam *ipAllocator
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
// IPAM — why the network declares a subnet and ephemerd owns allocation.
// DHCP IPAM is not available. Declaring no Ipams fails on metal (Server 2025
// 26100) at network creation:
//
//	hcnCreateNetwork failed in Win32: The network does not have a subnet for
//	this endpoint. (0x803b0005) / ErrorCode 2151350277
//
// so an L2Bridge network must carry a subnet plus a default route. Given one,
// HNS assigns endpoint addresses on its own — scattered anywhere across the
// declared prefix, which on a real LAN means straight into the site DHCP
// server's scope. ephemerd therefore pins each endpoint to an address it
// allocated out of the operator's reserved network.ip_pool (see setup and
// l2bridge.go). The subnet and the default-route next hop are read off the host
// adapter at runtime unless the operator pinned them; nothing here assumes any
// particular network.
func (w *windowsNetworking) initL2Bridge(cfg Config) error {
	// Fail closed on an incomplete plan: silently falling back to NAT would
	// defeat the egress guarantee the operator opted into, and guessing a pool
	// would hand out addresses that collide with live DHCP leases.
	plan, err := resolveL2BridgePlan(cfg, hostNetLookup{
		iface:   hostIPv4OnInterface,
		gateway: defaultGatewayForAdapter,
	})
	if err != nil {
		return fmt.Errorf("L2Bridge egress enabled but the address plan is incomplete: %w", err)
	}
	w.plan = plan
	w.ipam = newIPAllocator(plan.Pool, plan.HostIP, plan.Gateway)

	cfg.Log.Info("L2Bridge address plan resolved",
		"host_nic", cfg.HostNIC,
		"subnet", plan.Subnet, "subnet_derived", plan.DerivedSubnet,
		"gateway", plan.Gateway, "gateway_derived", plan.DerivedGateway,
		"host_ip", plan.HostIP,
		"ip_pool", plan.PoolSpec, "pool_size", plan.Pool.size())

	if cfg.AllowHostAccess {
		// Worth a warning on its own line: this is the one rule that lets a job
		// container address the ephemerd host at all.
		cfg.Log.Warn("L2Bridge egress permits containers to reach this host (required by dind / the module proxy)",
			"host_ip", plan.HostIP, "control_ports_blocked", cfg.ControlPorts)
	}

	if existing, err := hcn.GetNetworkByName(l2BridgeNetworkName); err == nil {
		w.network = existing
		w.adoptExistingEndpoints(existing)
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
		Ipams: []hcn.Ipam{
			{
				// "Static" means HNS does not run a DHCP client for the
				// endpoints; the addresses come from this subnet. ephemerd
				// narrows that further by pinning each endpoint itself.
				Type: "Static",
				Subnets: []hcn.Subnet{
					{
						IpAddressPrefix: plan.Subnet,
						Routes: []hcn.Route{
							{
								// The LAN router. Containers route THROUGH it
								// while the egress ACLs stop them ADDRESSING
								// it — verified working on metal.
								NextHop:           plan.Gateway,
								DestinationPrefix: "0.0.0.0/0",
							},
						},
					},
				},
			},
		},
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
		return fmt.Errorf("creating HCN L2Bridge network on %q (subnet %s, gateway %s): %w",
			cfg.HostNIC, plan.Subnet, plan.Gateway, err)
	}
	w.network = created

	cfg.Log.Info("HCN L2Bridge network created",
		"name", l2BridgeNetworkName, "id", created.Id, "host_nic", cfg.HostNIC,
		"subnet", plan.Subnet, "gateway", plan.Gateway)
	return nil
}

// adoptExistingEndpoints marks the addresses of endpoints already present on an
// adopted network as in use, so a daemon restart that finds leftover endpoints
// does not hand the same address to a new container.
//
// These reservations are held for the life of the process: ephemerd did not
// allocate them, so it has no id to release them under. The addresses return to
// the pool at the next restart, once the stale endpoints are gone. Size the pool
// with a little headroom rather than exactly runner.max_concurrent.
//
// Best-effort: a failure to enumerate is logged, not fatal — HNS rejects a
// duplicate address at endpoint creation anyway, which fails the job closed.
func (w *windowsNetworking) adoptExistingEndpoints(network *hcn.HostComputeNetwork) {
	eps, err := hcn.ListEndpointsOfNetwork(network.Id)
	if err != nil {
		w.cfg.Log.Warn("could not enumerate existing L2Bridge endpoints; pool may briefly double-allocate", "error", err)
		return
	}
	for _, ep := range eps {
		for _, ipc := range ep.IpConfigurations {
			if ipc.IpAddress != "" {
				w.ipam.reserve(ipc.IpAddress)
			}
		}
	}
	if len(eps) > 0 {
		w.cfg.Log.Info("reserved addresses of pre-existing L2Bridge endpoints", "endpoints", len(eps))
	}
}

// hostAddr reports the address containers use to reach services ephemerd hosts.
// On L2Bridge that is the host's own LAN address — there is no bridge gateway to
// bind to, and the NAT path's hard-coded 10.88.0.1 exists on no interface once
// the NAT network is out of the picture. Returning it here is what keeps the
// per-job dind listener (pkg/dind/listen_windows.go) and the Go module proxy
// bindable, and therefore jobs provisionable.
func (w *windowsNetworking) hostAddr() string {
	if w.cfg.L2BridgeEgress && w.plan != nil {
		return w.plan.HostIP
	}
	// NAT path: unchanged — the generic subnet derivation yields defaultGateway.
	return ""
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

	// Create endpoint on the network.
	//
	// On NAT, HNS assigns from the NAT subnet as before (no IpConfigurations).
	//
	// On L2Bridge the container is a peer on the host's LAN, so leaving the
	// choice to HNS means an address anywhere in the declared subnet — which on
	// a real LAN overlaps the site DHCP server's scope. ephemerd pins the
	// endpoint to an address it allocated from the operator's reserved
	// network.ip_pool instead, at the LAN's own prefix length so the container's
	// route table matches its neighbours'.
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

	var allocatedIP string
	if w.cfg.L2BridgeEgress {
		if w.ipam == nil || w.plan == nil {
			return nil, fmt.Errorf("L2Bridge egress enabled but no address plan was resolved for %s (refusing to let HNS pick an address that may collide with a DHCP lease)", id)
		}
		ip, err := w.ipam.allocate(id)
		if err != nil {
			return nil, fmt.Errorf("allocating an address for %s: %w", id, err)
		}
		allocatedIP = ip
		// PrefixLength is deliberately NOT set: HNS derives it from the
		// network's declared subnet. The 2026-08-12 hand proof pinned exactly
		// this shape and worked; Microsoft's own sdnbridge CNI also omits it.
		// Setting it was the delta in the dead deployment (gateway ARP replies
		// dropped by the vSwitch as "Invalid Packet" before reaching the port).
		endpoint.IpConfigurations = []hcn.IpConfig{
			{
				IpAddress: allocatedIP,
			},
		}
	}

	created, err := w.network.CreateEndpoint(endpoint)
	if err != nil {
		if allocatedIP != "" {
			w.ipam.release(id)
		}
		return nil, fmt.Errorf("creating HCN endpoint for %s: %w", id, err)
	}

	// Apply ACL policies to block private network access. On L2Bridge the VFP
	// Switch-ACL ladder applied here is the ONLY egress restriction that
	// enforces — there is no host-side firewall mechanism that can filter this
	// traffic (see the header in firewall_windows.go). A failure here therefore
	// means the container would run with unrestricted egress to the host LAN,
	// other RFC1918 services, and link-local metadata endpoints. Fail CLOSED:
	// tear down the endpoint we just created and refuse the job rather than start
	// a container we cannot firewall.
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
		w.releaseIP(id)
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
		w.releaseIP(id)
		return nil, fmt.Errorf("creating HCN namespace for %s: %w", id, err)
	}

	if err := hcn.AddNamespaceEndpoint(ns.Id, created.Id); err != nil {
		_ = ns.Delete()
		_ = created.Delete()
		w.releaseIP(id)
		return nil, fmt.Errorf("attaching endpoint to namespace for %s: %w", id, err)
	}

	w.cfg.Log.Debug("HCN endpoint created", "id", id, "endpoint", created.Id, "namespace", ns.Id, "ip", allocatedIP)
	return &SetupResult{NetNS: ns.Id, EndpointID: created.Id, IP: allocatedIP}, nil
}

// releaseIP returns a container's pool address, if it holds one. Safe on the NAT
// path (no allocator) and for containers that never got an address.
func (w *windowsNetworking) releaseIP(id string) {
	if w.ipam != nil {
		w.ipam.release(id)
	}
}

func (w *windowsNetworking) teardown(ctx context.Context, id string, netns string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Return the pool address whatever happens below: an endpoint that no
	// longer exists (or one we fail to delete) must not strand its address, or
	// a long-lived daemon would leak the pool one job at a time.
	defer w.releaseIP(id)

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
//
// CRITICAL — priorities below 100 kill the port (proven on metal 2026-08-14,
// l2test bisect harness, Server 2025 build 26100). A Switch-rule ACL with
// Priority < 100 silently kills the endpoint's ENTIRE VFP dataplane: the port
// drops every inbound frame — including the gateway's ARP reply (pktmon shows
// "Invalid Packet" at the vSwitch before the port) — so the container can
// never resolve its next hop and has no connectivity at all, while HNS reports
// the endpoint healthy and applies the policy without error. This is
// independent of the rule's action, direction, or address: an irrelevant
// Block 203.0.113.1/32 at priority 90 reproduces it, and the identical ladder
// with every priority >= 100 works (internet up, RFC1918 blocked). This is
// what broke the 2026-08-13 production deployment (host-allow at 90), and the
// earlier "port-scoped DHCP rules blackhole the port" incident (rules at
// 90/95) was in the same band. NO RULE MAY EVER CARRY Priority < 100; the
// regression tests enforce it.
const (
	// aclPriorityMinimum is the lowest Switch-ACL priority that is safe on
	// metal. Rules below this kill the port — see the block comment above.
	aclPriorityMinimum uint16 = 100
	// aclPriorityHostAllow carves the ephemerd host's own /32 out ABOVE the
	// RFC1918 block. Emitted only when Config.AllowHostAccess is set, which is
	// what makes the per-job dind Docker API and the module proxy reachable.
	aclPriorityHostAllow uint16 = 100
	// aclPriorityExtraAllow carves configured destinations out ABOVE the
	// RFC1918 block. Unused by default (no carve-outs) — reserved for future
	// operator-allowed destinations.
	aclPriorityExtraAllow uint16 = 150
	// aclPriorityBlock denies the RFC1918 + link-local supernets, whole. No
	// gateway/own-subnet carve-out: on L2Bridge the container is a LAN peer,
	// so carving the subnet would expose the management plane and the router.
	aclPriorityBlock uint16 = 200
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
//
// hostIP, when non-empty, adds one further carve-out: a /32 allow for the
// ephemerd host itself. Nothing ephemerd serves TO containers over the network
// works without it — the per-job dind Docker API and the Go module proxy both
// listen on the host address, and a container that cannot address the host
// cannot use either. It is deliberately an address-scoped /32 and not a
// port-scoped rule: see the port-scoping note above; scoping it to the dind
// port would blackhole the whole VFP port. The exposure that opens (every port
// the host has listening, not just ephemerd's) is closed back down at the host
// firewall by l2BridgeControlPlaneRules, which CAN match the container source
// on this path because L2Bridge does not NAT.
func buildL2BridgeEgressACLPolicies(extraAllowed []string, hostIP string) ([]hcn.EndpointPolicy, error) {
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

	// Tier: the ephemerd host's own /32, when something it serves must be
	// reachable (dind, the module proxy). Highest precedence in the ladder.
	if hostIP != "" {
		if err := add(hcn.AclPolicySetting{
			Protocols:       aclAnyProtocol,
			Action:          hcn.ActionTypeAllow,
			Direction:       hcn.DirectionTypeOut,
			RemoteAddresses: hostIP + "/32",
			RuleType:        hcn.RuleTypeSwitch,
			Priority:        aclPriorityHostAllow,
		}); err != nil {
			return nil, err
		}
	}

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

// applyACLPolicies applies the per-endpoint egress ACLs. Only the L2Bridge path
// has a working enforcement point: it applies the router-safe VFP ladder
// (buildL2BridgeEgressACLPolicies), built up front and applied atomically, with
// any failure returned so the caller (setup) can treat it as fatal for the job.
//
// On the NAT path there is nothing to apply: block ACLs on the NAT vSwitch port
// were inert on metal (they never filtered the NAT'd egress), and no host-side
// mechanism can filter NAT container egress on this stack (see the header in
// firewall_windows.go). NAT egress is unfiltered by design — installFirewallRules
// logs that gap and points the operator at network.l2bridge_egress.
func (w *windowsNetworking) applyACLPolicies(endpoint *hcn.HostComputeEndpoint) error {
	if !w.cfg.L2BridgeEgress {
		return nil
	}

	policies, err := buildL2BridgeEgressACLPolicies(w.cfg.ExtraAllowedCIDRs, w.hostAllowIP())
	if err != nil {
		return err
	}

	return endpoint.ApplyPolicy(hcn.RequestTypeAdd, hcn.PolicyEndpointRequest{
		Policies: policies,
	})
}

// hostAllowIP returns the host address to carve out of the egress block, or ""
// for the strict posture in which the host is unreachable like the rest of
// RFC1918. Non-empty only when the operator runs something containers must
// reach (dind, the module proxy) AND the address plan resolved.
func (w *windowsNetworking) hostAllowIP() string {
	if w.cfg.AllowHostAccess && w.plan != nil {
		return w.plan.HostIP
	}
	return ""
}

// installFirewallRules and removeFirewallRules live in firewall_windows.go
// (mirroring firewall_linux.go): the host-global Windows Firewall backstop
// that complements the per-endpoint ACLs applied above.

func (w *windowsNetworking) cleanup() {}

func cleanStaleBridge(_ *slog.Logger) {} // no-op on Windows (HCN, not CNI bridge)
