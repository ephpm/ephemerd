package networking

import (
	"fmt"
	"os/exec"
)

// Blocked private/link-local ranges — containers should not reach the homelab.
var deniedRanges = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16", // link-local / cloud metadata
}

// InstallFirewallRules adds iptables rules to block container traffic to
// private network ranges. The bridge subnet itself is allowed so containers
// can communicate with the gateway (for NAT).
//
// These rules are idempotent — they use -C (check) before -A (append).
func (m *Manager) InstallFirewallRules() error {
	for _, cidr := range deniedRanges {
		// Don't block the container subnet itself
		if cidr == DefaultSubnet {
			continue
		}

		rule := []string{
			"-t", "filter",
			"-A", "FORWARD",
			"-s", DefaultSubnet,
			"-d", cidr,
			"-j", "REJECT",
			"--reject-with", "icmp-net-unreachable",
		}

		// Check if rule already exists
		checkRule := make([]string, len(rule))
		copy(checkRule, rule)
		checkRule[2] = "-C" // change -A to -C
		if err := exec.Command("iptables", checkRule...).Run(); err == nil {
			continue // rule already exists
		}

		m.cfg.Log.Info("adding firewall rule", "action", "deny", "src", DefaultSubnet, "dst", cidr)
		if err := exec.Command("iptables", rule...).Run(); err != nil {
			return fmt.Errorf("adding iptables rule for %s: %w", cidr, err)
		}
	}

	m.cfg.Log.Info("firewall rules installed", "denied_ranges", deniedRanges)
	return nil
}

// RemoveFirewallRules cleans up the iptables rules added by InstallFirewallRules.
func (m *Manager) RemoveFirewallRules() {
	for _, cidr := range deniedRanges {
		if cidr == DefaultSubnet {
			continue
		}

		rule := []string{
			"-t", "filter",
			"-D", "FORWARD",
			"-s", DefaultSubnet,
			"-d", cidr,
			"-j", "REJECT",
			"--reject-with", "icmp-net-unreachable",
		}

		exec.Command("iptables", rule...).Run()
	}
}
