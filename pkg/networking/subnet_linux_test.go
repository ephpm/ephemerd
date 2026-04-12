//go:build linux

package networking

import "testing"

// --- deriveGateway tests ---

func TestDeriveGateway(t *testing.T) {
	tests := []struct {
		subnet string
		want   string
	}{
		{"10.88.0.0/16", "10.88.0.1"},
		{"10.0.0.0/16", "10.0.0.1"},
		{"192.168.1.0/24", "192.168.1.1"},
		{"172.16.0.0/12", "172.16.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.subnet, func(t *testing.T) {
			got := deriveGateway(tt.subnet)
			if got != tt.want {
				t.Errorf("deriveGateway(%q) = %q, want %q", tt.subnet, got, tt.want)
			}
		})
	}
}

func TestDeriveGateway_InvalidCIDR(t *testing.T) {
	got := deriveGateway("not-a-cidr")
	if got != "10.89.0.1" {
		t.Errorf("deriveGateway(invalid) = %q, want fallback %q", got, "10.89.0.1")
	}
}

// --- Config.subnet tests ---

func TestConfig_Subnet_CustomValue(t *testing.T) {
	cfg := Config{Subnet: "10.99.0.0/16"}
	if got := cfg.subnet(); got != "10.99.0.0/16" {
		t.Errorf("subnet() = %q, want %q", got, "10.99.0.0/16")
	}
}
