//go:build linux

package networking

import (
	"fmt"
	"os/exec"
)

const chainName = "EPHEMERD-FORWARD"

var deniedRanges = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
}

func iptables(args ...string) error {
	out, err := exec.Command("iptables", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, out)
	}
	return nil
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

	l.cfg.Log.Info("firewall rules installed")
	return nil
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

	// Delete the bridge interface so it doesn't conflict on next startup
	// (the subnet may change between runs)
	out, err := exec.Command("ip", "link", "delete", defaultBridgeName).CombinedOutput()
	if err != nil {
		l.cfg.Log.Debug("failed to delete bridge", "bridge", defaultBridgeName, "error", fmt.Sprintf("%s: %s", err, out))
	} else {
		l.cfg.Log.Info("bridge deleted", "bridge", defaultBridgeName)
	}
}
