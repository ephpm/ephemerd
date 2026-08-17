//go:build linux

package networking

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
)

const chainName = "EPHEMERD-FORWARD"

// inputChainName holds the container -> host (locally delivered) policy.
// See issue #156 for the finding this closes.
//
// EPHEMERD-FORWARD only governs traffic the host ROUTES for a container. A
// packet a container addresses to a local host address — the bridge gateway
// (10.88.0.1), the host's own LAN address, or any other address the host owns —
// is delivered locally: PREROUTING, routing decision, INPUT. It never reaches
// FORWARD. So every rule in EPHEMERD-FORWARD, including the GatewayPorts
// allow-list and the RFC1918 REJECTs, matches exactly zero of that traffic
// (confirmed on a live node: the tcp/8082 module-proxy "allow" had 0 packets
// after 9.8 GB of forwarded traffic, because the proxy is reached over INPUT).
//
// Before this chain existed the only INPUT rules ephemerd installed were the
// controlPlaneInputRules DROPs, and those are configured only on the in-VM
// worker path — a bare-metal Linux node installed no INPUT rules at all. With
// the distro default of "INPUT policy ACCEPT, no rules", every port on the
// gateway and every port on the host's LAN address was reachable from every
// job container, and GatewayPorts constrained nothing.
//
// This chain makes the GatewayPorts policy actually a policy: container ->
// local-address traffic is default-deny, with explicit allows for DNS to the
// gateway and for the configured gateway ports. It does not depend on the
// host's INPUT policy, because the deny is a rule in our own chain rather than
// an absence of rules.
const inputChainName = "EPHEMERD-INPUT"

// dindChainName holds the per-job allow/deny pairs for dind's TCP Docker API
// listeners. It is a chain of its own, jumped to from both INPUT and
// EPHEMERD-FORWARD, so that per-job rules can be added and removed
// independently of the static rule set that installFirewallRules lays down —
// re-running the installer must not revoke a live job's allow, and a job
// ending must not disturb the static rules.
//
// Both jump points are needed. A container packet addressed to the bridge
// gateway is delivered locally and traverses INPUT, never FORWARD, so INPUT is
// where the decision actually gets made today. EPHEMERD-FORWARD is covered as
// well because its "allow container-to-container" rule (the gateway address is
// inside the container subnet) would otherwise be a standing bypass on any
// host or kernel configuration where bridged traffic does reach FORWARD.
const dindChainName = "EPHEMERD-DIND"

var deniedRanges = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
}

// deniedRanges6 mirrors deniedRanges for IPv6. Without these the FORWARD
// filter is IPv4-only and a container with a global IPv6 address could reach
// IPv6 private/link-local space — including the IPv6 cloud metadata endpoint
// (fd00:ec2::254 on AWS). fc00::/7 covers ULA (the v6 analogue of RFC1918),
// fe80::/10 covers link-local.
var deniedRanges6 = []string{
	"fc00::/7",
	"fe80::/10",
}

func iptables(args ...string) error {
	out, err := exec.Command("iptables", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables: %w: %s", err, out)
	}
	return nil
}

func ip6tables(args ...string) error {
	out, err := exec.Command("ip6tables", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip6tables: %w: %s", err, out)
	}
	return nil
}

// controlPlaneInputRules returns the argv for each targeted INPUT DROP rule
// that blocks the container subnet from reaching the ephemerd control plane
// bound on the gateway (containerd, dispatch gRPC, debug exec). The rules are
// intentionally narrow — source = container subnet, destination = gateway IP,
// specific TCP dport — so container→gateway NAT and DNS (dport 53) are
// unaffected. Returns nil when no control ports are configured (bare-metal
// Linux with no in-VM dispatch server on the bridge).
//
// Exposed as a pure function (no side effects) so tests can assert the exact
// rule set without invoking iptables.
func controlPlaneInputRules(subnet, gateway string, ports []int) [][]string {
	if subnet == "" || gateway == "" || len(ports) == 0 {
		return nil
	}
	rules := make([][]string, 0, len(ports))
	for _, port := range ports {
		rules = append(rules, []string{
			"INPUT",
			"-s", subnet,
			"-d", gateway,
			"-p", "tcp",
			"--dport", fmt.Sprintf("%d", port),
			"-j", "DROP",
		})
	}
	return rules
}

// containerInputJumpRule is the INPUT rule that sends everything sourced from
// the container subnet into EPHEMERD-INPUT. Matching on source (rather than on
// the bridge interface) also catches a packet arriving from the LAN with a
// forged container source address, which would otherwise land in INPUT
// unfiltered.
func containerInputJumpRule(subnet string) []string {
	return []string{"INPUT", "-s", subnet, "-j", inputChainName}
}

// containerInputRules returns the argv for each rule inside EPHEMERD-INPUT, in
// evaluation order. This is the container -> host policy for locally delivered
// traffic, and it ends in a default DROP so it does not inherit whatever the
// host's INPUT policy happens to be.
//
// The allow-list is deliberately minimal — everything a Linux job container
// legitimately needs from the host it runs on:
//
//   - loopback, so the host talking to its own bridge address (the module proxy
//     binds there) is never caught by a rule aimed at containers. A container
//     cannot produce a packet on `lo`: it lives in its own netns and its
//     traffic arrives on the bridge.
//   - RELATED,ESTABLISHED, so replies to connections the HOST opened toward a
//     container come back. A container cannot reach a denied port through this:
//     the opening SYN is ctstate NEW, so it falls through to the DROP and the
//     flow never becomes ESTABLISHED.
//   - DNS to the gateway, matching the EPHEMERD-FORWARD policy exactly.
//   - each configured GatewayPorts port, to the gateway, TCP. This is the
//     allow-list the FORWARD chain was only pretending to enforce.
//
// Ports that are also ControlPorts are dropped from the allow-list rather than
// emitted: containerd, the unauthenticated dispatch gRPC server and the debug
// exec server must not become reachable because an operator listed one of them
// under network gateway ports. That makes the outcome independent of how the
// two INPUT rule sets happen to be ordered.
//
// Everything else — the rest of the gateway's ports, the host's own LAN
// address (which the FORWARD RFC1918 REJECTs never covered, because it is a
// local delivery), any other address the host owns, ICMP to the host — is
// dropped.
//
// Pure (no side effects) so the exact rule set is unit-testable without
// invoking iptables. Returns nil when there is no gateway to scope allows to.
func containerInputRules(gateway string, gatewayPorts, controlPorts []int) [][]string {
	if gateway == "" {
		return nil
	}

	rules := [][]string{
		{inputChainName, "-i", "lo", "-j", "ACCEPT"},
		{inputChainName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}

	// DNS to the bridge gateway, udp and tcp — the same carve-out the FORWARD
	// chain makes.
	for _, proto := range []string{"udp", "tcp"} {
		rules = append(rules, []string{
			inputChainName, "-d", gateway, "-p", proto, "--dport", "53", "-j", "ACCEPT",
		})
	}

	control := make(map[int]bool, len(controlPorts))
	for _, p := range controlPorts {
		control[p] = true
	}

	seen := map[int]bool{53: true} // already allowed above
	for _, port := range gatewayPorts {
		if port <= 0 || port > 65535 || control[port] || seen[port] {
			continue
		}
		seen[port] = true
		rules = append(rules, []string{
			inputChainName, "-d", gateway, "-p", "tcp", "--dport", fmt.Sprintf("%d", port), "-j", "ACCEPT",
		})
	}

	// Default deny. DROP rather than REJECT to match the posture of the
	// control-plane INPUT drops and to give a job no ICMP oracle for probing
	// which host services exist.
	rules = append(rules, []string{inputChainName, "-j", "DROP"})
	return rules
}

// dindPortRules returns the ordered rule pair that authorizes exactly one
// container to reach one dind Docker API port on the gateway.
//
// First an ACCEPT for the owning container's /32, then a DROP for everything
// else aimed at that port. Order is the whole mechanism: iptables takes the
// first match, so the /32 is carved out of a deny that is otherwise total.
//
// The DROP is deliberately not scoped to the container subnet. The port only
// ever needs to serve its one container, so denying every other source —
// other containers, other bridges, the host's own processes, anything that
// routes to the gateway address — is both free and strictly safer than
// enumerating who is excluded.
//
// Exposed as a pure function (no side effects) so tests can assert the exact
// rules and their order without invoking iptables.
func dindPortRules(gateway, containerIP string, port int) [][]string {
	dport := fmt.Sprintf("%d", port)
	return [][]string{
		{"-s", containerIP + "/32", "-d", gateway, "-p", "tcp", "--dport", dport, "-j", "ACCEPT"},
		{"-d", gateway, "-p", "tcp", "--dport", dport, "-j", "DROP"},
	}
}

// ensureDindChain creates EPHEMERD-DIND and its jumps if they are not already
// in place. Idempotent, and safe to call when installFirewallRules has already
// run — every step is check-before-add.
//
// Called from openHostPort rather than only from installFirewallRules so that
// the per-job allow cannot silently land in a chain nothing jumps to. A dind
// rule that is installed but unreachable would look like success while leaving
// the port open to every container.
func (l *linuxNetworking) ensureDindChain() error {
	// -N fails with EEXIST when the chain is already there; that is the
	// normal case after the first job, so the error is not interesting.
	_ = iptables("-N", dindChainName)

	if err := iptables("-C", "INPUT", "-j", dindChainName); err != nil {
		if err := iptables("-I", "INPUT", "1", "-j", dindChainName); err != nil {
			return fmt.Errorf("inserting %s jump into INPUT: %w", dindChainName, err)
		}
	}

	// EPHEMERD-FORWARD is created by installFirewallRules. If that has not
	// run (or failed), there is nothing to hook and nothing to protect there
	// — the INPUT jump above is the one that matters. Create it so the jump
	// has a home rather than failing the whole open.
	_ = iptables("-N", chainName)
	if err := iptables("-C", chainName, "-j", dindChainName); err != nil {
		if err := iptables("-I", chainName, "1", "-j", dindChainName); err != nil {
			return fmt.Errorf("inserting %s jump into %s: %w", dindChainName, chainName, err)
		}
	}
	return nil
}

// openHostPort authorizes exactly one container to reach one dind Docker API
// port on the bridge gateway, and denies it to everything else.
//
// SECURITY. This is not a convenience carve-out, it is the authenticator. The
// Docker API dind serves on this port accepts any request from anyone: it has
// no credentials, no TLS, no notion of which job is calling. Reaching the port
// is equivalent to full control of that job's daemon — run and exec
// containers, bind-mount host paths, read its build context. Linux containers
// share one bridge and can address the gateway freely (INPUT policy is ACCEPT
// and nothing else filters container→gateway), so without these rules every
// concurrent job could drive every other job's daemon. Scoping to the owning
// container's /32 is what makes the transport safe to use at all.
//
// FAIL CLOSED. An empty or unparseable containerIP returns an error and opens
// nothing; the caller is expected to fail the job. There is no wider fallback
// — losing docker in one job beats losing isolation in all of them.
func (l *linuxNetworking) openHostPort(port int, containerIP string) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("dind host port %d is out of range", port)
	}
	ip := net.ParseIP(strings.TrimSpace(containerIP))
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("dind host port %d: container IP %q is not a usable IPv4 address", port, containerIP)
	}
	if err := l.ensureDindChain(); err != nil {
		return err
	}

	gateway := deriveGateway(l.cfg.subnet())
	rules := dindPortRules(gateway, ip.String(), port)
	for i, rule := range rules {
		if err := iptables(append([]string{"-A", dindChainName}, rule...)...); err != nil {
			// Roll back whatever landed: a lone ACCEPT with no DROP behind it
			// is the pool-wide hole this function exists to prevent.
			for j := 0; j < i; j++ {
				if delErr := iptables(append([]string{"-D", dindChainName}, rules[j]...)...); delErr != nil {
					l.cfg.Log.Warn("failed to roll back partial dind port allow",
						"port", port, "container_ip", ip.String(), "error", delErr)
				}
			}
			return fmt.Errorf("adding dind port rule (tcp/%d for %s): %w", port, ip.String(), err)
		}
	}
	l.cfg.Log.Info("dind API port scoped to owning container",
		"port", port, "container_ip", ip.String(), "gateway", gateway)
	return nil
}

// closeHostPort removes the pair openHostPort added. Best-effort: the rules
// are also flushed wholesale when the daemon tears the firewall down, so a
// failure here leaks at worst until the next restart, and only for a port
// nothing is listening on any more.
func (l *linuxNetworking) closeHostPort(port int, containerIP string) {
	ip := net.ParseIP(strings.TrimSpace(containerIP))
	if ip == nil || ip.To4() == nil {
		l.cfg.Log.Debug("skipping dind port close: unusable container IP",
			"port", port, "container_ip", containerIP)
		return
	}
	gateway := deriveGateway(l.cfg.subnet())
	for _, rule := range dindPortRules(gateway, ip.String(), port) {
		if err := iptables(append([]string{"-D", dindChainName}, rule...)...); err != nil {
			l.cfg.Log.Debug("failed to remove dind port rule",
				"port", port, "container_ip", ip.String(), "error", err)
		}
	}
}

func (l *linuxNetworking) installFirewallRules() error {
	// Create our own chain to avoid interference from CNI/Netavark
	_ = iptables("-N", chainName)

	// Flush in case of leftovers from a previous run
	_ = iptables("-F", chainName)

	// Jump to our chain from FORWARD — insert at position 1 so we go first
	if err := iptables("-C", "FORWARD", "-s", l.cfg.subnet(), "-j", chainName); err != nil {
		l.cfg.Log.Info("adding jump to EPHEMERD-FORWARD chain")
		if err := iptables("-I", "FORWARD", "1", "-s", l.cfg.subnet(), "-j", chainName); err != nil {
			return fmt.Errorf("inserting jump rule: %w", err)
		}
	}

	// Also catch return traffic
	if err := iptables("-C", "FORWARD", "-d", l.cfg.subnet(), "-j", chainName); err != nil {
		if err := iptables("-I", "FORWARD", "1", "-d", l.cfg.subnet(), "-j", chainName); err != nil {
			return fmt.Errorf("inserting return jump rule: %w", err)
		}
	}

	// Rules inside our chain (evaluated in order, top to bottom):

	// 0. Per-job dind Docker API rules. Must be first: rule 2 below allows
	//    container-to-container traffic across the whole subnet, and the
	//    gateway address sits inside that subnet, so anything appended after
	//    it could never deny a container reaching a dind port. The chain is
	//    empty until a job opens a port; see ensureDindChain.
	if err := l.ensureDindChain(); err != nil {
		return err
	}
	// Drop per-job rules left by a previous daemon that did not shut down
	// cleanly. This runs at startup, before any job exists, so nothing live
	// is being revoked — whereas a stale DROP for a port number the kernel
	// later hands to a new job would silently break that job's docker.
	if err := iptables("-F", dindChainName); err != nil {
		l.cfg.Log.Debug("failed to flush dind chain", "error", err)
	}

	// 1. Allow return traffic
	l.cfg.Log.Info("adding firewall rule", "rule", "allow return traffic")
	if err := iptables("-A", chainName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("adding return traffic rule: %w", err)
	}

	// 2. Allow container-to-container
	l.cfg.Log.Info("adding firewall rule", "rule", "allow container-to-container")
	if err := iptables("-A", chainName, "-s", l.cfg.subnet(), "-d", l.cfg.subnet(), "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("adding container-to-container rule: %w", err)
	}

	// 3. Allow DNS only to the bridge gateway (prevents DNS tunneling to private networks)
	gateway := deriveGateway(l.cfg.subnet())
	for _, proto := range []string{"udp", "tcp"} {
		l.cfg.Log.Info("adding firewall rule", "rule", "allow DNS "+proto+" to gateway")
		if err := iptables("-A", chainName, "-p", proto, "-d", gateway, "--dport", "53", "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("adding DNS rule (%s): %w", proto, err)
		}
	}

	// 3b. Allow extra gateway ports (e.g., Go module proxy).
	//     Note this is NOT where the gateway-port policy is enforced —
	//     gateway-addressed traffic is delivered locally and never traverses
	//     FORWARD. See EPHEMERD-INPUT below for the rules that actually
	//     govern it. These are kept so the FORWARD chain stays a complete
	//     statement of intent and so a future routed (non-local) gateway
	//     would still be covered.
	for _, port := range l.cfg.GatewayPorts {
		l.cfg.Log.Info("adding firewall rule", "rule", fmt.Sprintf("allow tcp/%d to gateway", port))
		if err := iptables("-A", chainName, "-p", "tcp", "-d", gateway, "--dport", fmt.Sprintf("%d", port), "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("adding gateway port rule (tcp/%d): %w", port, err)
		}
	}

	// 4. Deny private ranges
	for _, cidr := range deniedRanges {
		l.cfg.Log.Info("adding firewall rule", "rule", "deny "+cidr)
		if err := iptables("-A", chainName, "-d", cidr, "-j", "REJECT", "--reject-with", "icmp-net-unreachable"); err != nil {
			return fmt.Errorf("adding deny rule for %s: %w", cidr, err)
		}
	}

	// 5. Allow everything else (internet)
	l.cfg.Log.Info("adding firewall rule", "rule", "allow outbound")
	if err := iptables("-A", chainName, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("adding outbound rule: %w", err)
	}

	// 6. Container → host policy, on the INPUT path.
	//
	//    Everything above governs only what the host ROUTES for a container.
	//    A packet addressed to a local address — the bridge gateway, the
	//    host's own LAN address, anything else the host owns — is delivered
	//    locally and never sees FORWARD, so none of the rules above (including
	//    the GatewayPorts allow-list and the RFC1918 REJECTs) apply to it.
	//    EPHEMERD-INPUT default-denies that traffic and re-states the
	//    GatewayPorts allow-list where it actually takes effect.
	if err := l.installContainerInputChain(gateway); err != nil {
		return err
	}

	// 7. Block the container subnet from reaching the ephemerd control plane
	//    (containerd / dispatch gRPC / debug exec) bound on the gateway.
	//    Redundant with the default DROP in EPHEMERD-INPUT and kept anyway:
	//    these are inserted at INPUT position 1 AFTER the jump above, so they
	//    sit above it and deny the control ports outright, independent of the
	//    chain below. Narrow (subnet→gateway, specific TCP dport) so NAT and
	//    DNS stay intact.
	for _, rule := range controlPlaneInputRules(l.cfg.subnet(), gateway, l.cfg.ControlPorts) {
		port := rule[len(rule)-3] // "--dport <port> -j DROP" → port is 3 from end
		l.cfg.Log.Info("adding firewall rule", "rule", "drop container→gateway control port "+port)
		// Idempotent: check-before-insert. Insert (not append) so the DROP
		// precedes any permissive INPUT rule an earlier subsystem installed.
		if err := iptables(append([]string{"-C"}, rule...)...); err != nil {
			insert := append([]string{"-I", rule[0], "1"}, rule[1:]...)
			if err := iptables(insert...); err != nil {
				return fmt.Errorf("adding control-plane INPUT drop (tcp/%s): %w", port, err)
			}
		}
	}

	// 8. IPv6: mirror the private/link-local FORWARD denies and the
	//    control-plane INPUT drops. IPv6 was previously entirely unfiltered,
	//    so a container that inherited host v6 forwarding could reach v6
	//    ULA/link-local space and the v6 cloud metadata endpoint. Best-effort:
	//    on hosts with IPv6 disabled these fail harmlessly and are logged,
	//    not fatal — IPv4 firewalling is already in place at this point.
	for _, r := range ipv6FirewallRules(l.cfg.ControlPorts) {
		// Idempotent check-before-insert. The -C check uses the chain + match
		// args (no insert position); the apply uses -A (append) or -I <chain>
		// 1 (insert first) per the rule's Insert flag.
		if err := ip6tables(append([]string{"-C", r.chain}, r.match...)...); err == nil {
			continue // already present
		}
		var apply []string
		if r.insert {
			apply = append([]string{"-I", r.chain, "1"}, r.match...)
		} else {
			apply = append([]string{"-A", r.chain}, r.match...)
		}
		if err := ip6tables(apply...); err != nil {
			l.cfg.Log.Warn("ip6tables rule not installed (IPv6 may be disabled)",
				"chain", r.chain, "match", strings.Join(r.match, " "), "error", err)
		}
	}

	l.cfg.Log.Info("firewall rules installed")
	return nil
}

// installContainerInputChain creates EPHEMERD-INPUT, fills it with the
// container -> host policy, and jumps to it from INPUT for anything sourced
// from the container subnet.
//
// The chain is flushed and rebuilt every install so a config change (adding or
// removing a gateway port) takes effect on restart rather than accumulating
// stale allows. The jump is check-before-insert so restarts do not stack
// duplicates.
//
// No IPv6 mirror, deliberately: the CNI bridge is IPv4-only, so containers
// have no v6 address and no v6 path to the host. A v6 analogue would have to
// match by destination (there is no v6 container subnet to scope a source to)
// and would therefore default-deny the host's own link-local/ULA traffic,
// breaking NDP on any dual-stack node — a real outage traded for a hole that
// does not exist.
func (l *linuxNetworking) installContainerInputChain(gateway string) error {
	rules := containerInputRules(gateway, l.cfg.GatewayPorts, l.cfg.ControlPorts)
	if len(rules) == 0 {
		return nil
	}

	_ = iptables("-N", inputChainName)
	if err := iptables("-F", inputChainName); err != nil {
		return fmt.Errorf("flushing %s: %w", inputChainName, err)
	}

	for _, rule := range rules {
		if err := iptables(append([]string{"-A"}, rule...)...); err != nil {
			return fmt.Errorf("adding %s rule (%s): %w", inputChainName, strings.Join(rule[1:], " "), err)
		}
	}

	// Jump last, so the chain is never half-built while INPUT already points
	// at it — a partially built chain ends at the implicit RETURN, not at the
	// DROP, which would fail open for as long as the install took.
	jump := containerInputJumpRule(l.cfg.subnet())
	if err := iptables(append([]string{"-C"}, jump...)...); err != nil {
		if err := iptables(append([]string{"-I", jump[0], "1"}, jump[1:]...)...); err != nil {
			return fmt.Errorf("inserting %s jump rule: %w", inputChainName, err)
		}
	}

	l.cfg.Log.Info("container→host INPUT policy installed (default deny)",
		"chain", inputChainName,
		"gateway", gateway,
		"allowed_gateway_ports", l.cfg.GatewayPorts,
		"dns", true)
	return nil
}

// removeContainerInputChain tears down what installContainerInputChain built.
func (l *linuxNetworking) removeContainerInputChain() {
	jump := containerInputJumpRule(l.cfg.subnet())
	if err := iptables(append([]string{"-D", jump[0]}, jump[1:]...)...); err != nil {
		l.cfg.Log.Debug("failed to remove container INPUT jump rule", "error", err)
	}
	if err := iptables("-F", inputChainName); err != nil {
		l.cfg.Log.Debug("failed to flush chain", "chain", inputChainName, "error", err)
	}
	if err := iptables("-X", inputChainName); err != nil {
		l.cfg.Log.Debug("failed to delete chain", "chain", inputChainName, "error", err)
	}
}

// ip6Rule is a single ip6tables rule split into chain, match args, and
// whether it should be inserted first (INPUT drops) or appended (FORWARD
// denies). Kept structured so both the -C idempotency check and the apply
// step derive from the same match args.
type ip6Rule struct {
	chain  string
	match  []string
	insert bool
}

// ipv6FirewallRules returns the IPv6 firewall rules that mirror the IPv4
// posture. There is no configured IPv6 container subnet (the CNI bridge is
// IPv4-only), so the FORWARD denies match by DESTINATION only — safe because
// the bridge only forwards our containers' traffic — and the control-plane
// INPUT drops match by destination (a v6 gateway would live in link-local/ULA
// space) plus TCP dport. Exposed as a pure function so tests can assert the
// emitted set without invoking ip6tables.
func ipv6FirewallRules(controlPorts []int) []ip6Rule {
	var rules []ip6Rule

	// Deny v6 private/link-local destinations in FORWARD (cloud metadata,
	// ULA, link-local).
	for _, cidr := range deniedRanges6 {
		rules = append(rules, ip6Rule{
			chain: "FORWARD",
			match: []string{"-d", cidr, "-j", "REJECT", "--reject-with", "icmp6-adm-prohibited"},
		})
	}

	// Drop v6 traffic to the control ports destined to link-local/ULA (where
	// a v6 gateway address would sit). Inserted first in INPUT.
	for _, port := range controlPorts {
		for _, cidr := range deniedRanges6 {
			rules = append(rules, ip6Rule{
				chain:  "INPUT",
				match:  []string{"-d", cidr, "-p", "tcp", "--dport", fmt.Sprintf("%d", port), "-j", "DROP"},
				insert: true,
			})
		}
	}

	return rules
}

func (l *linuxNetworking) removeFirewallRules() {
	// Remove the per-job dind chain and its jumps. The EPHEMERD-FORWARD jump
	// goes away with that chain below, but the INPUT one lives in a built-in
	// chain and has to be deleted by hand or it dangles across restarts.
	if err := iptables("-D", "INPUT", "-j", dindChainName); err != nil {
		l.cfg.Log.Debug("failed to remove dind jump from INPUT", "error", err)
	}
	if err := iptables("-F", dindChainName); err != nil {
		l.cfg.Log.Debug("failed to flush dind chain", "error", err)
	}

	// Remove jump rules from FORWARD
	if err := iptables("-D", "FORWARD", "-s", l.cfg.subnet(), "-j", chainName); err != nil {
		l.cfg.Log.Debug("failed to remove forward jump rule", "error", err)
	}
	if err := iptables("-D", "FORWARD", "-d", l.cfg.subnet(), "-j", chainName); err != nil {
		l.cfg.Log.Debug("failed to remove return jump rule", "error", err)
	}

	// Flush and delete our chain
	if err := iptables("-F", chainName); err != nil {
		l.cfg.Log.Debug("failed to flush chain", "error", err)
	}
	if err := iptables("-X", chainName); err != nil {
		l.cfg.Log.Debug("failed to delete chain", "error", err)
	}
	// Delete the dind chain only after EPHEMERD-FORWARD is gone — the jump
	// from it is a reference that -X would otherwise refuse to drop.
	if err := iptables("-X", dindChainName); err != nil {
		l.cfg.Log.Debug("failed to delete dind chain", "error", err)
	}

	// Remove the container→host INPUT policy chain and its jump.
	l.removeContainerInputChain()

	// Remove the IPv4 control-plane INPUT drops (they live in the built-in
	// INPUT chain, not our own chain, so -F/-X above doesn't touch them).
	gateway := deriveGateway(l.cfg.subnet())
	for _, rule := range controlPlaneInputRules(l.cfg.subnet(), gateway, l.cfg.ControlPorts) {
		del := append([]string{"-D", rule[0]}, rule[1:]...)
		if err := iptables(del...); err != nil {
			l.cfg.Log.Debug("failed to remove control-plane INPUT drop", "error", err)
		}
	}

	// Remove the IPv6 rules (FORWARD denies + INPUT control-plane drops).
	// Best-effort; on hosts without IPv6 these were never installed.
	for _, r := range ipv6FirewallRules(l.cfg.ControlPorts) {
		del := append([]string{"-D", r.chain}, r.match...)
		if err := ip6tables(del...); err != nil {
			l.cfg.Log.Debug("failed to remove ip6tables rule", "chain", r.chain, "error", err)
		}
	}

	// Delete the bridge interface so it doesn't conflict on next startup
	// (the subnet may change between runs)
	out, err := exec.Command("ip", "link", "delete", defaultBridgeName).CombinedOutput()
	if err != nil {
		l.cfg.Log.Debug("failed to delete bridge", "bridge", defaultBridgeName, "error", fmt.Sprintf("%s: %s", err, out))
	} else {
		l.cfg.Log.Info("bridge deleted", "bridge", defaultBridgeName)
	}
}
