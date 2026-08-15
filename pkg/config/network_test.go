package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The addresses here are RFC 5737 documentation ranges. Neither these tests nor
// the shipped defaults encode any real site's LAN — network.ip_pool has no
// default at all, which is the point of most of this file.

// loadNetworkTOML writes a minimal valid config with the given [network] block
// and runs it through Load, returning the validation error (if any).
func loadNetworkTOML(t *testing.T, networkBlock string) (*Config, error) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "ghp_test123")

	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[github]\nowner = \"testorg\"\n\n[network]\n" + networkBlock
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// TestNetworkValidate_L2BridgeRequiresIPPool is the fail-fast the whole L2Bridge
// IPAM design rests on. On L2Bridge, containers take addresses on the operator's
// own LAN; ephemerd must never guess a range, because a guess collides with live
// DHCP leases. Opting in without ip_pool has to stop the daemon at startup with
// a message naming the key.
func TestNetworkValidate_L2BridgeRequiresIPPool(t *testing.T) {
	_, err := loadNetworkTOML(t, "l2bridge_egress = true\nhost_nic = \"Ethernet\"\n")
	if err == nil {
		t.Fatal("Load accepted l2bridge_egress = true with no ip_pool; want a startup failure")
	}
	msg := err.Error()
	for _, want := range []string{"network.ip_pool", "l2bridge_egress", "DHCP"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	// The message must show the operator what a value looks like, in
	// documentation address space — never a real LAN.
	if !strings.Contains(msg, "192.0.2.") {
		t.Errorf("error %q gives no example value", msg)
	}
}

// TestNetworkValidate_L2BridgeRequiresHostNIC covers the other setting that
// cannot be inferred: which adapter to bridge onto.
func TestNetworkValidate_L2BridgeRequiresHostNIC(t *testing.T) {
	_, err := loadNetworkTOML(t, "l2bridge_egress = true\nip_pool = \"192.0.2.192/27\"\n")
	if err == nil {
		t.Fatal("Load accepted l2bridge_egress = true with no host_nic")
	}
	if !strings.Contains(err.Error(), "network.host_nic") {
		t.Errorf("error %q does not name network.host_nic", err)
	}
}

// TestNetworkValidate_L2BridgeMinimalConfig verifies the intended surface: two
// required keys, everything else derived from the host at runtime.
func TestNetworkValidate_L2BridgeMinimalConfig(t *testing.T) {
	cfg, err := loadNetworkTOML(t, "l2bridge_egress = true\nhost_nic = \"Ethernet\"\nip_pool = \"192.0.2.192/27\"\n")
	if err != nil {
		t.Fatalf("Load rejected a minimal valid L2Bridge config: %v", err)
	}
	if !cfg.Network.L2BridgeEgress || cfg.Network.HostNIC != "Ethernet" || cfg.Network.IPPool != "192.0.2.192/27" {
		t.Errorf("parsed [network] = %+v, want the three keys round-tripped", cfg.Network)
	}
	// Everything optional stays empty so the runtime derives it from the host's
	// real adapter. A non-empty default here would be a hardcoded network.
	if cfg.Network.Subnet != "" || cfg.Network.Gateway != "" || len(cfg.Network.PublicDNS) != 0 {
		t.Errorf("optional keys defaulted to %q / %q / %v, want empty (derived at runtime)",
			cfg.Network.Subnet, cfg.Network.Gateway, cfg.Network.PublicDNS)
	}
}

// TestNetworkValidate_L2BridgeOptionalOverrides verifies the escape hatches
// parse when an operator needs to pin them.
func TestNetworkValidate_L2BridgeOptionalOverrides(t *testing.T) {
	cfg, err := loadNetworkTOML(t, `l2bridge_egress = true
host_nic = "Ethernet"
ip_pool = "192.0.2.200-192.0.2.230"
subnet = "192.0.2.0/24"
gateway = "192.0.2.1"
public_dns = ["9.9.9.9"]
extra_allowed_destinations = ["203.0.113.0/24"]
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n := cfg.Network
	if n.IPPool != "192.0.2.200-192.0.2.230" || n.Subnet != "192.0.2.0/24" || n.Gateway != "192.0.2.1" {
		t.Errorf("address keys = %+v, want the configured values", n)
	}
	if len(n.PublicDNS) != 1 || n.PublicDNS[0] != "9.9.9.9" {
		t.Errorf("public_dns = %v, want [9.9.9.9]", n.PublicDNS)
	}
	if len(n.ExtraAllowedDestinations) != 1 || n.ExtraAllowedDestinations[0] != "203.0.113.0/24" {
		t.Errorf("extra_allowed_destinations = %v", n.ExtraAllowedDestinations)
	}
}

// TestNetworkValidate_NotRequiredWhenOptedOut pins that the default path — NAT,
// and every Linux/macOS node — is untouched by any of this.
func TestNetworkValidate_NotRequiredWhenOptedOut(t *testing.T) {
	if _, err := loadNetworkTOML(t, "mtu = 1400\n"); err != nil {
		t.Fatalf("Load rejected a config that never opts into L2Bridge: %v", err)
	}
	if _, err := loadNetworkTOML(t, "l2bridge_egress = false\n"); err != nil {
		t.Fatalf("Load rejected l2bridge_egress = false: %v", err)
	}
}

// TestNetworkValidate_BlankStringsAreNotSet guards against a rendered config
// that emits the keys with empty values passing validation.
func TestNetworkValidate_BlankStringsAreNotSet(t *testing.T) {
	_, err := loadNetworkTOML(t, "l2bridge_egress = true\nhost_nic = \"  \"\nip_pool = \"192.0.2.192/27\"\n")
	if err == nil {
		t.Error("whitespace-only host_nic accepted")
	}
	_, err = loadNetworkTOML(t, "l2bridge_egress = true\nhost_nic = \"Ethernet\"\nip_pool = \"  \"\n")
	if err == nil {
		t.Error("whitespace-only ip_pool accepted")
	}
}
