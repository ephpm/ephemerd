package networking

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
)

// Address planning for the Windows L2Bridge egress path.
//
// This file is deliberately platform-independent: it holds the pure address
// arithmetic (pool parsing, allocation, host-network derivation, validation) so
// the whole plan is unit-testable on any OS, while network_windows.go keeps the
// HNS calls.
//
// WHY AN EXPLICIT POOL IS REQUIRED
//
// On L2Bridge, containers are not behind NAT — they are peers on the host's own
// LAN. HNS refuses to create an L2Bridge network that declares no Ipam subnet:
//
//	hcnCreateNetwork failed in Win32: The network does not have a subnet for
//	this endpoint. (0x803b0005)
//
// so "just DHCP" is not an option; the network must carry a subnet. Given one,
// HNS will happily assign endpoint addresses itself — from anywhere in that
// subnet, which on a real LAN means colliding with the site DHCP server's
// scope. Measured on metal: four container runs landed on four unrelated
// addresses scattered across the declared prefix.
//
// So ephemerd allocates the addresses itself, out of an operator-declared range
// (network.ip_pool) that the site's DHCP server is configured not to hand out,
// and pins each endpoint with an explicit IpConfiguration. There is deliberately
// NO default pool: any built-in guess would either be wrong for the operator's
// LAN or would quietly hand out addresses that DHCP also leases. Missing config
// fails fast at startup instead.

// ipRange is an inclusive IPv4 address range, held as host-order uint32 so pool
// arithmetic is exact and cheap.
type ipRange struct {
	lo, hi uint32
}

func (r ipRange) size() uint64 { return uint64(r.hi) - uint64(r.lo) + 1 }

func (r ipRange) contains(v uint32) bool { return v >= r.lo && v <= r.hi }

// containsRange reports whether other lies entirely inside r.
func (r ipRange) containsRange(other ipRange) bool {
	return other.lo >= r.lo && other.hi <= r.hi
}

func (r ipRange) String() string { return u32ToIPv4(r.lo) + "-" + u32ToIPv4(r.hi) }

// ipv4ToU32 converts a dotted-quad to host-order uint32.
func ipv4ToU32(ip net.IP) (uint32, bool) {
	v4 := ip.To4()
	if v4 == nil {
		return 0, false
	}
	return binary.BigEndian.Uint32(v4), true
}

// u32ToIPv4 renders a host-order uint32 back to dotted-quad form.
func u32ToIPv4(v uint32) string {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).String()
}

// parseIPPool parses an address pool in either supported form:
//
//	"192.0.2.32-192.0.2.63"  explicit inclusive start-end range
//	"192.0.2.32/27"          CIDR (network and broadcast addresses excluded)
//
// Both forms are IPv4 only — the Windows container stack ephemerd drives has no
// IPv6 path. A CIDR of /31 or /32 has no network/broadcast to reserve and is
// used whole.
func parseIPPool(spec string) (ipRange, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return ipRange{}, fmt.Errorf("empty address pool")
	}

	if start, end, ok := strings.Cut(s, "-"); ok {
		lo, err := parseIPv4(strings.TrimSpace(start))
		if err != nil {
			return ipRange{}, fmt.Errorf("pool %q: start address: %w", spec, err)
		}
		hi, err := parseIPv4(strings.TrimSpace(end))
		if err != nil {
			return ipRange{}, fmt.Errorf("pool %q: end address: %w", spec, err)
		}
		if hi < lo {
			return ipRange{}, fmt.Errorf("pool %q: end address is below the start address", spec)
		}
		return ipRange{lo: lo, hi: hi}, nil
	}

	if !strings.Contains(s, "/") {
		return ipRange{}, fmt.Errorf("pool %q: want a CIDR (192.0.2.32/27) or a start-end range (192.0.2.32-192.0.2.63)", spec)
	}

	_, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return ipRange{}, fmt.Errorf("pool %q: %w", spec, err)
	}
	base, ok := ipv4ToU32(ipnet.IP)
	if !ok {
		return ipRange{}, fmt.Errorf("pool %q: not an IPv4 CIDR", spec)
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return ipRange{}, fmt.Errorf("pool %q: not an IPv4 mask", spec)
	}
	lo, hi := base, base|(1<<(32-ones)-1)
	if ones <= 30 {
		// Skip the network and broadcast addresses; handing either to a
		// container would be a guaranteed-broken endpoint.
		lo, hi = lo+1, hi-1
	}
	return ipRange{lo: lo, hi: hi}, nil
}

// parseIPv4 parses a bare dotted-quad address.
func parseIPv4(s string) (uint32, error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return 0, fmt.Errorf("%q is not an IP address", s)
	}
	v, ok := ipv4ToU32(ip)
	if !ok {
		return 0, fmt.Errorf("%q is not an IPv4 address", s)
	}
	return v, nil
}

// ipAllocator hands out addresses from an ipRange, one per container endpoint.
//
// ephemerd owns allocation on the L2Bridge path (rather than letting HNS pick)
// precisely so every container address stays inside the operator's reserved
// range. Allocation is lowest-free-first and idempotent per id, so a retried
// setup for the same container reuses its address instead of leaking one.
type ipAllocator struct {
	mu       sync.Mutex
	pool     ipRange
	reserved map[uint32]struct{} // never handed out (host, gateway, adopted endpoints)
	byID     map[string]uint32
	inUse    map[uint32]string
}

// newIPAllocator builds an allocator over pool. Addresses in reserved are
// permanently withheld; malformed entries are ignored (callers pass addresses
// they have already validated).
func newIPAllocator(pool ipRange, reserved ...string) *ipAllocator {
	a := &ipAllocator{
		pool:     pool,
		reserved: map[uint32]struct{}{},
		byID:     map[string]uint32{},
		inUse:    map[uint32]string{},
	}
	for _, r := range reserved {
		a.reserve(r)
	}
	return a
}

// reserve withholds an address from future allocations. Used for the host and
// gateway addresses, and for endpoints adopted from a previous daemon run.
// A non-IPv4 or out-of-pool address is a no-op.
func (a *ipAllocator) reserve(addr string) {
	v, err := parseIPv4(strings.TrimSpace(addr))
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reserved[v] = struct{}{}
}

// allocate returns the lowest free address in the pool for id, or an error when
// the pool is exhausted. Callers must treat exhaustion as fatal for the job:
// starting a container without an address of our choosing means either no
// network or an HNS-picked address that may collide with a DHCP lease.
func (a *ipAllocator) allocate(id string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if v, ok := a.byID[id]; ok {
		return u32ToIPv4(v), nil
	}

	for v := a.pool.lo; ; v++ {
		_, isReserved := a.reserved[v]
		_, isUsed := a.inUse[v]
		if !isReserved && !isUsed {
			a.byID[id] = v
			a.inUse[v] = id
			return u32ToIPv4(v), nil
		}
		if v == a.pool.hi {
			break // written this way so a pool ending at 255.255.255.255 cannot wrap
		}
	}

	return "", fmt.Errorf("address pool %s exhausted (%d in use): raise network.ip_pool or lower runner.max_concurrent", a.pool, len(a.inUse))
}

// release returns id's address to the pool. Safe to call for an unknown id.
func (a *ipAllocator) release(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v, ok := a.byID[id]; ok {
		delete(a.byID, id)
		delete(a.inUse, v)
	}
}

// l2BridgePlan is the fully resolved address plan for the L2Bridge network:
// everything HNS needs, plus the host address containers use to reach services
// ephemerd hosts (dind, the module proxy).
type l2BridgePlan struct {
	// Subnet is the LAN CIDR the HNS network declares in its Ipam. Required —
	// HNS rejects a subnet-less L2Bridge network outright (0x803b0005).
	Subnet string
	// PrefixLen is Subnet's prefix length, stamped onto each endpoint's
	// IpConfiguration so the container's route table matches the real LAN.
	PrefixLen int
	// Gateway is the LAN router, used as the Ipam default-route next hop.
	// Containers route THROUGH it while the egress ACLs stop them ADDRESSING
	// it — that combination was verified on metal.
	Gateway string
	// HostIP is the ephemerd host's own address on HostNIC.
	HostIP string
	// Pool is the operator-declared range container addresses come from.
	Pool ipRange
	// PoolSpec is the pool exactly as configured, kept for firewall rules and
	// log lines that should echo the operator's own words.
	PoolSpec string
	// DerivedSubnet / DerivedGateway record whether each value was read off the
	// host rather than configured, so startup can log what it inferred.
	DerivedSubnet  bool
	DerivedGateway bool
}

// hostNetLookup abstracts the two host queries the plan needs, so
// resolveL2BridgePlan is testable without a Windows host or a real NIC.
type hostNetLookup struct {
	// iface returns the host's IPv4 address and its network on the named
	// adapter.
	iface func(name string) (net.IP, *net.IPNet, error)
	// gateway returns the default-route next hop reachable via the named
	// adapter.
	gateway func(name string) (string, error)
}

// resolveL2BridgePlan turns the operator's [network] config plus the host's
// actual NIC state into a complete address plan, or a specific error naming the
// key that needs setting.
//
// Nothing here defaults to a particular network. subnet and gateway are read off
// the host's real primary adapter when the operator has not pinned them; ip_pool
// has no default at all, because only the operator knows which addresses their
// DHCP server is configured never to lease.
func resolveL2BridgePlan(cfg Config, look hostNetLookup) (*l2BridgePlan, error) {
	nic := strings.TrimSpace(cfg.HostNIC)
	if nic == "" {
		return nil, fmt.Errorf("network.host_nic is required when network.l2bridge_egress = true: " +
			"set it to the host adapter to bridge onto (the name shown by `Get-NetAdapter`, e.g. \"Ethernet\"). " +
			"There is no default — the correct adapter is host-specific")
	}
	if strings.TrimSpace(cfg.IPPool) == "" {
		return nil, errIPPoolRequired
	}

	hostIP, hostNet, err := look.iface(nic)
	if err != nil {
		return nil, err
	}
	hostU32, ok := ipv4ToU32(hostIP)
	if !ok {
		return nil, fmt.Errorf("adapter %q: host address %s is not IPv4 (network.l2bridge_egress is IPv4 only)", nic, hostIP)
	}

	plan := &l2BridgePlan{HostIP: u32ToIPv4(hostU32)}

	// Subnet: the operator's if pinned, otherwise the adapter's own prefix.
	subnetSpec := strings.TrimSpace(cfg.Subnet)
	if subnetSpec == "" {
		subnetSpec = hostNet.String()
		plan.DerivedSubnet = true
	}
	subnetRange, prefixLen, err := cidrRange(subnetSpec)
	if err != nil {
		return nil, fmt.Errorf("network.subnet %q: %w", subnetSpec, err)
	}
	if !subnetRange.contains(hostU32) {
		return nil, fmt.Errorf("network.subnet %s does not contain the address %s configured on adapter %q: "+
			"either clear network.subnet to derive it from the adapter, or correct it to the LAN the adapter is on",
			subnetSpec, plan.HostIP, nic)
	}
	plan.Subnet = subnetSpec
	plan.PrefixLen = prefixLen

	// Gateway: the operator's if pinned, otherwise the adapter's default route.
	gwSpec := strings.TrimSpace(cfg.Gateway)
	if gwSpec == "" {
		if look.gateway == nil {
			return nil, errGatewayRequired(nic, fmt.Errorf("no default-route lookup available on this platform"))
		}
		gwSpec, err = look.gateway(nic)
		if err != nil {
			return nil, errGatewayRequired(nic, err)
		}
		gwSpec = strings.TrimSpace(gwSpec)
		if gwSpec == "" {
			return nil, errGatewayRequired(nic, fmt.Errorf("adapter has no IPv4 default route"))
		}
		plan.DerivedGateway = true
	}
	gwU32, err := parseIPv4(gwSpec)
	if err != nil {
		return nil, fmt.Errorf("network.gateway: %w", err)
	}
	if !subnetRange.contains(gwU32) {
		return nil, fmt.Errorf("network.gateway %s is outside network.subnet %s: the L2Bridge default route "+
			"next hop must be an on-link address of the bridged LAN", gwSpec, plan.Subnet)
	}
	plan.Gateway = u32ToIPv4(gwU32)

	// Pool: must be inside the subnet and must not swallow the host or router.
	pool, err := parseIPPool(cfg.IPPool)
	if err != nil {
		return nil, fmt.Errorf("network.ip_pool: %w", err)
	}
	if !subnetRange.containsRange(pool) {
		return nil, fmt.Errorf("network.ip_pool %s (%s) is not inside network.subnet %s: containers are LAN peers "+
			"on L2Bridge, so the pool must be a reserved slice of the bridged LAN", cfg.IPPool, pool, plan.Subnet)
	}
	if pool.contains(hostU32) {
		return nil, fmt.Errorf("network.ip_pool %s contains this host's own address %s: pick a range that excludes it",
			cfg.IPPool, plan.HostIP)
	}
	if pool.contains(gwU32) {
		return nil, fmt.Errorf("network.ip_pool %s contains the LAN gateway %s: pick a range that excludes it",
			cfg.IPPool, plan.Gateway)
	}
	plan.Pool = pool
	plan.PoolSpec = strings.TrimSpace(cfg.IPPool)

	return plan, nil
}

// errIPPoolRequired is the backstop for the one setting that cannot be
// inferred. pkg/config rejects a missing ip_pool at load time, before any HNS
// object exists, with the full explanation; this catches a networking.Config
// assembled by any other caller.
var errIPPoolRequired = fmt.Errorf(
	`network.ip_pool is required when network.l2bridge_egress = true: ` +
		`set it to a slice of your LAN that DHCP never leases, as a CIDR ("192.0.2.192/27") ` +
		`or an inclusive range ("192.0.2.200-192.0.2.230"). There is no default`)

// errGatewayRequired reports that the LAN default route could not be derived and
// tells the operator which key pins it manually.
func errGatewayRequired(nic string, cause error) error {
	return fmt.Errorf("could not derive the LAN default gateway for adapter %q (%w): "+
		"set network.gateway explicitly to the router address the L2Bridge should route through", nic, cause)
}

// cidrRange returns a CIDR's inclusive address range and prefix length.
func cidrRange(cidr string) (ipRange, int, error) {
	_, ipnet, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return ipRange{}, 0, err
	}
	base, ok := ipv4ToU32(ipnet.IP)
	if !ok {
		return ipRange{}, 0, fmt.Errorf("not an IPv4 CIDR")
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return ipRange{}, 0, fmt.Errorf("not an IPv4 mask")
	}
	return ipRange{lo: base, hi: base | (1<<(32-ones) - 1)}, ones, nil
}

// hostIPv4OnInterface returns the first routable IPv4 address configured on the
// named adapter, together with its network. On Windows the adapter name is the
// friendly name shown by Get-NetAdapter, which is also what the HNS
// NetAdapterName network policy expects — the same string in both places.
func hostIPv4OnInterface(name string) (net.IP, *net.IPNet, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, nil, fmt.Errorf("network.host_nic %q: no adapter with that name on this host "+
			"(check `Get-NetAdapter`): %w", name, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, nil, fmt.Errorf("network.host_nic %q: reading adapter addresses: %w", name, err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		v4 := ipnet.IP.To4()
		if v4 == nil || v4.IsLoopback() || v4.IsLinkLocalUnicast() {
			continue
		}
		// Normalize to a 4-byte IP and a /n IPv4 mask.
		ones, bits := ipnet.Mask.Size()
		if bits != 32 {
			continue
		}
		return v4, &net.IPNet{IP: v4.Mask(ipnet.Mask), Mask: net.CIDRMask(ones, 32)}, nil
	}
	return nil, nil, fmt.Errorf("network.host_nic %q: adapter has no routable IPv4 address "+
		"(an APIPA/link-local-only adapter cannot bridge); give the adapter a static or DHCP address, "+
		"or set network.host_nic to the adapter that carries the LAN", name)
}
