//go:build linux

package networking

import (
	"fmt"
	"os/exec"
)

var deniedRanges = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
}

func (l *linuxNetworking) installFirewallRules() error {
	for _, cidr := range deniedRanges {
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

		checkRule := make([]string, len(rule))
		copy(checkRule, rule)
		checkRule[2] = "-C"
		if err := exec.Command("iptables", checkRule...).Run(); err == nil {
			continue
		}

		l.cfg.Log.Info("adding firewall rule", "action", "deny", "src", DefaultSubnet, "dst", cidr)
		if err := exec.Command("iptables", rule...).Run(); err != nil {
			return fmt.Errorf("adding iptables rule for %s: %w", cidr, err)
		}
	}

	l.cfg.Log.Info("firewall rules installed", "denied_ranges", deniedRanges)
	return nil
}

func (l *linuxNetworking) removeFirewallRules() {
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
