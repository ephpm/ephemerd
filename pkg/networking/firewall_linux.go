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
	// Allow container-to-container traffic within the ephemerd subnet.
	// This must come before the broad 10.0.0.0/8 deny rule which is a
	// superset of the container subnet.
	allowRule := []string{
		"-t", "filter",
		"-A", "FORWARD",
		"-s", DefaultSubnet,
		"-d", DefaultSubnet,
		"-j", "ACCEPT",
	}
	checkAllow := make([]string, len(allowRule))
	copy(checkAllow, allowRule)
	checkAllow[2] = "-C"
	if err := exec.Command("iptables", checkAllow...).Run(); err != nil {
		l.cfg.Log.Info("adding firewall rule", "action", "allow", "src", DefaultSubnet, "dst", DefaultSubnet)
		if err := exec.Command("iptables", allowRule...).Run(); err != nil {
			return fmt.Errorf("adding iptables allow rule for container subnet: %w", err)
		}
	}

	for _, cidr := range deniedRanges {
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
		rule := []string{
			"-t", "filter",
			"-D", "FORWARD",
			"-s", DefaultSubnet,
			"-d", cidr,
			"-j", "REJECT",
			"--reject-with", "icmp-net-unreachable",
		}

		_ = exec.Command("iptables", rule...).Run()
	}

	// Remove the container subnet allow rule
	exec.Command("iptables",
		"-t", "filter",
		"-D", "FORWARD",
		"-s", DefaultSubnet,
		"-d", DefaultSubnet,
		"-j", "ACCEPT",
	).Run()
}
