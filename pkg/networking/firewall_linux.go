//go:build linux

package networking

import (
	"fmt"
	"os/exec"
	"strings"
)

const chainName = "EPHEMERD-FORWARD"

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

	// 3b. Allow extra gateway ports (e.g., Go module proxy)
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

	// 6. Block the container subnet from reaching the ephemerd control plane
	//    (containerd / dispatch gRPC / debug exec) bound on the gateway. The
	//    FORWARD chain above only governs container→off-host traffic; traffic
	//    addressed to the gateway IP itself is delivered locally and hits the
	//    INPUT chain, so it must be dropped there. These rules are narrow
	//    (subnet→gateway, specific TCP dport) so NAT and DNS stay intact.
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

	// 7. IPv6: mirror the private/link-local FORWARD denies and the
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
