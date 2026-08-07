//go:build windows

package networking

import (
	"encoding/binary"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// Host-side egress firewall for Windows job containers.
//
// Two layers restrict what a Windows job can reach:
//
//  1. Per-endpoint HNS ACL policies (applyACLPolicies in network_windows.go) —
//     VFP rules on the container's vSwitch port, applied in setup().
//  2. The host-global Windows Defender Firewall rules installed here.
//
// Layer 2 exists because layer 1 alone left the fleet reachable in practice:
// the containment suite (.github/workflows/containment.yml, "Fleet management
// planes must be unreachable") reached the Incus daemon and Grafana from a
// Hyper-V-isolated job (#135), so per-endpoint vSwitch ACLs cannot be the only
// line of defense. Every container flow is routed and NATed by the host
// network stack (WinNAT), so host firewall rules sit in that path regardless
// of what the vSwitch port enforces — and, like the Linux FORWARD chain, they
// are host-global: one rule set covers every endpoint, including stale ones
// leaked by a crashed run. HNS network-level ACLs were considered instead but
// rejected: they use the same VFP enforcement point as the endpoint ACLs that
// just failed, and they cannot express "this range minus the gateway".
//
// Windows Firewall has no rule ordering and a Block rule always overrides an
// Allow rule, so the Linux pattern "allow the gateway above the RFC1918 deny"
// cannot be ported literally. Instead the container subnet — which contains
// the NAT gateway (DNS, the default route, module-proxy GatewayPorts) and the
// other containers — is subtracted from each blocked range up front
// (subtractCIDR), so it never appears in any block rule. Blocking the gateway
// would brick all container networking: DNS and outbound NAT both go through
// it.
//
// The outbound blocks are scoped localip=<container subnet>: forwarded
// container traffic is evaluated pre-NAT with its container source address,
// so the host's own traffic (sourced from the host LAN address) can never
// match — a mis-scoped rule here must degrade to a no-op, never to cutting
// the fleet host off its own management LAN.
//
// IPv4 only, deliberately: the HCN NAT network is IPv4-only (no v6 IPAM), so
// containers have no IPv6 path, and a host-wide v6 link-local block without a
// container-source scope would break the HOST's neighbor discovery.

// firewallRulePrefix names every host-firewall rule ephemerd installs so the
// set is findable (netsh advfirewall firewall show rule name=all | findstr
// ephemerd-egress) and removable on Cleanup.
const firewallRulePrefix = "ephemerd-egress"

func netsh(args ...string) error {
	out, err := exec.Command("netsh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh: %w: %s", err, out)
	}
	return nil
}

// winFirewallRule is one host-firewall rule: its unique name (used for the
// idempotent delete-before-add and for removal on Cleanup) and the key=value
// spec that creates it.
type winFirewallRule struct {
	name string
	spec []string
}

// addArgs returns the netsh argv that creates the rule.
func (r winFirewallRule) addArgs() []string {
	return append([]string{"advfirewall", "firewall", "add", "rule", "name=" + r.name}, r.spec...)
}

// deleteArgs returns the netsh argv that deletes every rule with this name.
func (r winFirewallRule) deleteArgs() []string {
	return []string{"advfirewall", "firewall", "delete", "rule", "name=" + r.name}
}

// hostFirewallRules returns the full host-firewall rule set for the given
// container subnet, gateway, and control-plane ports: outbound blocks for
// every denied range (with the container subnet carved out) plus inbound
// blocks for container→gateway traffic on the control ports.
//
// Exposed as a pure function (no side effects) so tests can assert the exact
// rule set without invoking netsh.
func hostFirewallRules(subnet, gateway string, controlPorts []int) ([]winFirewallRule, error) {
	var rules []winFirewallRule

	// Outbound RFC1918 + link-local blocks. The container subnet is subtracted
	// from any overlapping range (see the ordering note above: an allow rule
	// cannot outrank a block rule, so the gateway and the container-to-container
	// range must never appear inside a blocked range in the first place).
	// Everything outside these ranges — the internet — is untouched.
	for _, cidr := range egressBlockedCIDRs {
		remote, err := subtractCIDR(cidr, subnet)
		if err != nil {
			return nil, fmt.Errorf("computing blocked ranges for %s: %w", cidr, err)
		}
		if len(remote) == 0 {
			continue // fully covered by the container subnet
		}
		rules = append(rules, winFirewallRule{
			name: firewallRulePrefix + "-block-" + strings.ReplaceAll(cidr, "/", "_"),
			spec: []string{
				"dir=out",
				"action=block",
				"protocol=any",
				"localip=" + subnet,
				"remoteip=" + strings.Join(remote, ","),
				"profile=any",
				"enable=yes",
			},
		})
	}

	// Inbound control-plane blocks: container subnet → gateway on the ephemerd
	// control ports (containerd, dispatch gRPC, debug exec). Intentionally
	// narrow — source = container subnet, destination = gateway, one TCP port
	// each — so DNS (53) and NAT stay intact. Mirrors controlPlaneInputRules
	// on Linux; traffic addressed to the gateway IP terminates at the host, so
	// the outbound blocks above never see it.
	for _, port := range controlPorts {
		rules = append(rules, winFirewallRule{
			name: fmt.Sprintf("%s-control-%d", firewallRulePrefix, port),
			spec: []string{
				"dir=in",
				"action=block",
				"protocol=TCP",
				"localip=" + gateway,
				"localport=" + strconv.Itoa(port),
				"remoteip=" + subnet,
				"profile=any",
				"enable=yes",
			},
		})
	}

	return rules, nil
}

// subtractCIDR removes exclude from cidr and renders the remainder in netsh
// remoteip syntax: the original CIDR when the two do not overlap, otherwise up
// to two "start-end" ranges. Returns an empty slice when exclude covers cidr
// entirely. IPv4 only — the HCN NAT network has no IPv6 IPAM.
func subtractCIDR(cidr, exclude string) ([]string, error) {
	clo, chi, err := v4Range(cidr)
	if err != nil {
		return nil, err
	}
	xlo, xhi, err := v4Range(exclude)
	if err != nil {
		return nil, err
	}

	if xhi < clo || xlo > chi {
		return []string{cidr}, nil // no overlap — keep the CIDR as-is
	}

	var out []string
	if xlo > clo {
		out = append(out, u32ToIP(clo)+"-"+u32ToIP(xlo-1))
	}
	if xhi < chi {
		out = append(out, u32ToIP(xhi+1)+"-"+u32ToIP(chi))
	}
	return out, nil
}

// v4Range returns the first and last address of an IPv4 CIDR as uint32.
func v4Range(cidr string) (lo, hi uint32, err error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0, 0, fmt.Errorf("parsing %s: %w", cidr, err)
	}
	ip4 := ipnet.IP.To4()
	if ip4 == nil {
		return 0, 0, fmt.Errorf("parsing %s: not an IPv4 CIDR", cidr)
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return 0, 0, fmt.Errorf("parsing %s: not an IPv4 mask", cidr)
	}
	lo = binary.BigEndian.Uint32(ip4)
	hi = lo | (1<<(32-ones) - 1)
	return lo, hi, nil
}

// u32ToIP renders a uint32 back to dotted-quad form.
func u32ToIP(v uint32) string {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).String()
}

func (w *windowsNetworking) installFirewallRules() error {
	// init() always creates the HCN network on DefaultSubnet with
	// defaultGateway (cfg.Subnet is not consulted on Windows), so the firewall
	// must match those, not cfg.Subnet.
	rules, err := hostFirewallRules(DefaultSubnet, defaultGateway, w.cfg.ControlPorts)
	if err != nil {
		return fmt.Errorf("building host firewall rules: %w", err)
	}

	for _, r := range rules {
		// Idempotent: delete any rule carrying this name from a previous run
		// before adding, so re-running install never accumulates duplicates.
		// netsh delete removes every rule matching the name; "no rules match"
		// on a fresh host is expected and ignored.
		_ = netsh(r.deleteArgs()...)

		w.cfg.Log.Info("adding firewall rule", "rule", r.name)
		if err := netsh(r.addArgs()...); err != nil {
			// Callers treat this as a warning, not fatal (see
			// cmd/ephemerd/main.go): a host where the daemon lacks the
			// privilege to program the firewall degrades to the per-endpoint
			// ACLs instead of refusing to start.
			return fmt.Errorf("adding firewall rule %s: %w", r.name, err)
		}
	}

	w.cfg.Log.Info("host firewall rules installed", "rules", len(rules))
	return nil
}

func (w *windowsNetworking) removeFirewallRules() {
	// Recompute the same deterministic rule set install built and delete each
	// rule by name. Best-effort, like the Linux removal path.
	rules, err := hostFirewallRules(DefaultSubnet, defaultGateway, w.cfg.ControlPorts)
	if err != nil {
		w.cfg.Log.Debug("failed to rebuild firewall rule set for removal", "error", err)
		return
	}
	for _, r := range rules {
		if err := netsh(r.deleteArgs()...); err != nil {
			w.cfg.Log.Debug("failed to remove firewall rule", "rule", r.name, "error", err)
		}
	}
}
