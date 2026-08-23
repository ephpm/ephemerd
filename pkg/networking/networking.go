package networking

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"path/filepath"
)

// DefaultSubnet is the preferred IP range for containers.
const DefaultSubnet = "10.88.0.0/16"

// cniConfListName is the filename of the CNI conflist the Linux networking
// backend writes under <DataDir>/cni/conf. Kept here (not just in
// network_linux.go) so callers on any platform can derive the path without a
// build constraint.
const cniConfListName = "10-ephemerd.conflist"

// CNIConfListPath returns the path to the CNI conflist for the given data dir.
//
// Exposed so the embedded BuildKit worker can point its network provider at the
// SAME bridge/subnet this package configures, ensuring build `RUN` steps land
// on the firewalled container network instead of the host netns. The file is
// only written on Linux (network_linux.go); on other platforms the path is
// meaningful only as a lookup key and nothing consumes it.
func CNIConfListPath(dataDir string) string {
	return filepath.Join(dataDir, "cni", "conf", cniConfListName)
}

// Config for container networking.
type Config struct {
	DataDir   string
	Subnet    string // container subnet (auto-selected if empty)
	MTU       int    // bridge MTU (auto-detected from host if 0)
	CNIBinDir string // path to CNI plugin binaries (Linux only, ignored elsewhere)

	// GatewayPorts are the extra TCP ports on the host/gateway address that a
	// container may reach (e.g. the Go module proxy). On Linux this is a true
	// allow-list: EPHEMERD-INPUT default-denies container→host traffic and
	// admits only DNS plus these ports. Anything listed here is exposed to
	// hostile job code, so list only services meant for jobs. Ports that are
	// also ControlPorts are ignored rather than opened.
	GatewayPorts []int

	// ControlPorts are TCP ports the ephemerd control plane binds on the
	// gateway (bridge) address that MUST NOT be reachable from inside
	// containers: the in-VM containerd (default 10000), the unauthenticated
	// dispatch gRPC server (containerd+1), and the debug exec server
	// (containerd+2). The firewall adds targeted INPUT DROP rules from the
	// container subnet to the gateway on these ports. Empty = no INPUT
	// control-plane rules (e.g. a bare-metal Linux host with no in-VM
	// dispatch server listening on the bridge).
	ControlPorts []int

	// The fields below configure the Windows L2Bridge egress path (see
	// network_windows.go and l2bridge.go). They are ignored on Linux/macOS.
	// When L2BridgeEgress is false (the default), Windows uses the HNS NAT
	// network and none of them are consulted.
	//
	// L2BridgeEgress is the opt-in. HostNIC (the host adapter to bridge onto)
	// and IPPool (the reserved LAN range container addresses come from) are
	// REQUIRED when it is set; Subnet and Gateway are derived from HostNIC when
	// empty. PublicDNS defaults to public resolvers so container DNS never needs
	// the blocked LAN router. ExtraAllowedCIDRs carves destinations out above
	// the RFC1918 block.
	L2BridgeEgress    bool
	HostNIC           string
	IPPool            string
	Gateway           string
	PublicDNS         []string
	ExtraAllowedCIDRs []string

	// AllowHostAccess permits job containers to address the ephemerd host
	// itself on the L2Bridge path. Required by anything ephemerd serves TO
	// containers over the network — the per-job dind Docker API and the Go
	// module proxy both listen on the host address — because the egress ACLs
	// otherwise block the host along with the rest of RFC1918.
	//
	// It is an address-scoped /32 allow, so it opens every port the host has
	// listening, not just ephemerd's. The control-plane ports are blocked back
	// off at the host firewall (see l2BridgeControlPlaneRules), which CAN match
	// on the container source here because L2Bridge does not NAT. Left false
	// when nothing needs to be reachable, which is the strictest posture.
	AllowHostAccess bool

	Log *slog.Logger
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
	// IP is the container's IP address on the container network. Populated on
	// Linux from the CNI result, and on Windows from the HCN endpoint — the
	// address ephemerd pinned on the L2Bridge path, the one HNS allocated on
	// the NAT path. Empty on macOS.
	//
	// Not informational: it is the scope of the per-job dind host-firewall
	// allow (see OpenHostPort). An empty IP on a platform that needs one costs
	// the job its Docker access, by design.
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

	// hostAddr returns the host address containers reach ephemerd's own
	// services on, when the platform knows it better than GatewayIP's
	// subnet arithmetic does. Empty means "use the generic derivation".
	//
	// This exists for the Windows L2Bridge path, where containers are LAN
	// peers and the reachable host address is the host's own adapter address
	// — not the .1 of any container subnet.
	hostAddr() string

	// openHostPort / closeHostPort open and close a scoped host-firewall
	// inbound allow for one TCP port, from ONE container to the host.
	//
	// Needed on BOTH Windows paths, because on Windows the dind Docker API is
	// served over TCP on a host address (runhcs supports neither a bind-mounted
	// unix socket nor named-pipe sharing), so a container reaching its own dind
	// is making an INBOUND connection to the host — which the host's Windows
	// Firewall default-denies. On L2Bridge the allow is scoped to the host's LAN
	// address; on NAT to the bridge gateway (10.88.0.1). Without it the SYN is
	// dropped and every docker command in the job dies with an i/o timeout
	// (#162). Also used on the Linux VM-isolated (Kata) path, where the job
	// container likewise reaches dind over TCP.
	//
	// containerIP is the address of the container the port is being opened FOR,
	// and the allow is scoped to that /32 — never to the whole pool. The dind
	// API behind these ports is unauthenticated, so a pool-scoped allow would
	// let any job container reach any other job's Docker daemon. An unknown or
	// malformed containerIP is an error, not a reason to widen the scope.
	//
	// Scoped to remoteip=<containerIP>/32, localport=<port> so ONLY that
	// service opens — RDP/SMB/RPC stay blocked. No-op on macOS.
	openHostPort(port int, containerIP string) error
	closeHostPort(port int, containerIP string)

	// joinJobNetwork / leaveJobNetwork implement per-job container-to-container
	// isolation on the shared CNI bridge. joinJobNetwork records that the
	// container attached under cniID belongs to jobID at containerIP and allows
	// it to reach — and be reached by — the OTHER containers of the same job
	// (the runner, its dind siblings, its `services:` containers), while the
	// bridge's default posture denies it any other job's containers.
	// leaveJobNetwork reverses that for the container attached under cniID.
	//
	// cniID is the SAME id passed to setup/teardown, so teardown needs only
	// that id — not the job or the address. Linux enforces this with iptables;
	// Windows already scopes per-endpoint egress ACLs and macOS has no bridge,
	// so both are no-ops.
	joinJobNetwork(cniID, jobID, containerIP string) error
	leaveJobNetwork(cniID string)
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

// GatewayIP returns the host address that services ephemerd runs for jobs must
// bind to in order to be reachable from inside containers — the Go module proxy
// and, on Windows, the per-job dind Docker API listener.
//
// Normally that is the bridge gateway (e.g. "10.88.0.1"), the first usable
// address of the container subnet. On the Windows L2Bridge path there is no
// such bridge gateway: containers are peers on the host's LAN, so the platform
// reports the host's own adapter address instead. Binding to the old hard-coded
// 10.88.0.1 there would fail outright — no interface holds that address once the
// NAT network is out of the picture — and take dind provisioning down with it.
func (m *Manager) GatewayIP() string {
	if m.platform != nil {
		if addr := m.platform.hostAddr(); addr != "" {
			return addr
		}
	}

	return gatewayForSubnet(m.cfg.Subnet)
}

// fallbackGateway is the address returned when no usable container subnet is
// configured. It is the first usable address of DefaultSubnet, and the value
// the Windows NAT network is created with (network_windows.go).
const fallbackGateway = "10.88.0.1"

// gatewayForSubnet returns the bridge gateway address for a container subnet:
// its first usable address (x.x.x.1). Empty or unparseable input falls back to
// fallbackGateway.
//
// Split out of GatewayIP because the Windows NAT host-port allow must be scoped
// to exactly the address dind bound its listener to. Two independent
// derivations of "the gateway" that drifted apart would produce a firewall rule
// for an address nothing is listening on — an allow that silently admits
// nothing, which is precisely the failure mode of #162.
func gatewayForSubnet(subnet string) string {
	if subnet == "" {
		subnet = DefaultSubnet
	}
	ip, _, err := net.ParseCIDR(subnet)
	if err != nil {
		return fallbackGateway
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return fallbackGateway
	}
	ip4[3] = 1
	return ip4.String()
}

// InstallFirewallRules blocks container traffic to private network ranges.
func (m *Manager) InstallFirewallRules() error {
	return m.platform.installFirewallRules()
}

// OpenHostPort opens a scoped host-firewall inbound allow for one TCP port from
// the single container at containerIP to the host, so a per-job service
// ephemerd binds on the host (dind's Docker API) is reachable from THAT job's
// container and no other. Both Windows paths (L2Bridge and the default NAT
// network) and the Linux VM-isolated path install a rule; macOS is a no-op.
// Pair with CloseHostPort on teardown, passing the same containerIP.
//
// Returns an error rather than opening anything when containerIP is empty or
// unparseable — see the platformNetworking doc for why widening is not a
// permissible fallback.
func (m *Manager) OpenHostPort(port int, containerIP string) error {
	if m.platform == nil {
		return nil
	}
	return m.platform.openHostPort(port, containerIP)
}

// CloseHostPort removes an allow previously added by OpenHostPort. containerIP
// must match the one it was opened with.
func (m *Manager) CloseHostPort(port int, containerIP string) {
	if m.platform != nil {
		m.platform.closeHostPort(port, containerIP)
	}
}

// JoinJobNetwork registers the container attached under cniID as belonging to
// jobID at containerIP and opens intra-job container-to-container reachability
// for it, while the bridge's default posture keeps it from reaching any other
// job's containers. Call it after Setup has returned the container's IP and
// before the container starts doing work. Pair with LeaveJobNetwork, passing
// the same cniID. No-op when no platform is initialized.
func (m *Manager) JoinJobNetwork(cniID, jobID, containerIP string) error {
	if m.platform == nil {
		return nil
	}
	return m.platform.joinJobNetwork(cniID, jobID, containerIP)
}

// LeaveJobNetwork reverses JoinJobNetwork for the container attached under
// cniID, removing the intra-job allows it held. Best-effort and safe to call
// for an id that never joined. Call it on teardown, alongside Teardown.
func (m *Manager) LeaveJobNetwork(cniID string) {
	if m.platform != nil {
		m.platform.leaveJobNetwork(cniID)
	}
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
