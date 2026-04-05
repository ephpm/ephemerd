//go:build darwin

package networking

import (
	"context"
)

// darwinNetworking is a passthrough on macOS.
//
// Container networking runs inside the Linux VM (same CNI bridge + iptables
// as native Linux). The macOS host doesn't need to configure networking —
// Virtualization.framework provides NAT for the VM, and the VM handles
// everything inside.
//
// This stub exists so ephemerd compiles on macOS without the CNI or HCN
// dependencies. The actual networking calls go through the containerd
// instance running inside the VM, which uses the Linux CNI path.
type darwinNetworking struct {
	cfg Config
}

func newPlatformNetworking() platformNetworking {
	return &darwinNetworking{}
}

func (d *darwinNetworking) init(cfg Config) error {
	d.cfg = cfg
	cfg.Log.Info("networking delegated to Linux VM (Virtualization.framework provides NAT to VM, CNI runs inside)")
	return nil
}

func (d *darwinNetworking) setup(_ context.Context, _ string, _ string) (*SetupResult, error) {
	// Networking is handled by containerd + CNI inside the VM.
	// The runtime calls containerd's API which does the CNI setup internally.
	return &SetupResult{}, nil
}

func (d *darwinNetworking) teardown(_ context.Context, _ string, _ string) error {
	return nil
}

func (d *darwinNetworking) installFirewallRules() error {
	// Firewall rules (iptables) run inside the Linux VM, configured
	// by the VM's init system using the same rules as native Linux.
	return nil
}

func (d *darwinNetworking) removeFirewallRules() {}
func (d *darwinNetworking) cleanup()             {}
