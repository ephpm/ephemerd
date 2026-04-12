package runtime

import (
	"runtime"
	"strings"
	"testing"
)

// --- isRoutableDNS tests ---

func TestIsRoutableDNS(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		// Public DNS — routable
		{"1.1.1.1", true},
		{"8.8.8.8", true},
		{"8.8.4.4", true},
		{"9.9.9.9", true},
		{"208.67.222.222", true},

		// 10.x.x.x — blocked by firewall
		{"10.0.0.1", false},
		{"10.255.255.254", false},
		{"10.88.0.1", false},

		// 172.16-31.x — blocked
		{"172.16.0.1", false},
		{"172.31.255.255", false},
		// 172.0-15 and 172.32+ are public
		{"172.15.0.1", true},
		{"172.32.0.1", true},

		// 192.168.x — blocked
		{"192.168.0.1", false},
		{"192.168.1.1", false},
		// 192.0.x is public
		{"192.0.2.1", true},

		// 169.254.x — link-local, blocked
		{"169.254.0.1", false},

		// 127.x — loopback, blocked
		{"127.0.0.1", false},
		{"127.0.0.53", false},

		// IPv6 — passes through (can't parse as IPv4)
		{"::1", true},
		{"2001:db8::1", true},

		// Edge cases
		{"", true},     // empty, let it through
		{"abc", true},  // non-IP, let it through
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			if got := isRoutableDNS(tt.ip); got != tt.want {
				t.Errorf("isRoutableDNS(%q) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

// --- buildResolvConf tests ---

func TestBuildResolvConf_FallbackOnMissingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		// /etc/resolv.conf doesn't exist on Windows, should fall back
		conf := buildResolvConf()
		if !strings.Contains(conf, "1.1.1.1") {
			t.Errorf("expected fallback DNS 1.1.1.1, got: %s", conf)
		}
		if !strings.Contains(conf, "8.8.8.8") {
			t.Errorf("expected fallback DNS 8.8.8.8, got: %s", conf)
		}
		return
	}
	// On Linux/macOS, /etc/resolv.conf likely exists
	conf := buildResolvConf()
	if conf == "" {
		t.Fatal("buildResolvConf() returned empty string")
	}
	if !strings.Contains(conf, "nameserver") {
		t.Errorf("expected at least one nameserver line, got: %s", conf)
	}
}

func TestBuildResolvConf_EndsWithNewline(t *testing.T) {
	conf := buildResolvConf()
	if !strings.HasSuffix(conf, "\n") {
		t.Errorf("buildResolvConf() should end with newline, got: %q", conf)
	}
}

// --- buildToTag tests (Windows image selection) ---

func TestBuildToTag(t *testing.T) {
	tests := []struct {
		name  string
		major uint32
		minor uint32
		build uint32
		want  string
	}{
		{"Windows Server 2025", 10, 0, 26100, "ltsc2025"},
		{"Windows 11 24H2", 10, 0, 26200, "ltsc2025"},
		{"Windows Server 2022", 10, 0, 20348, "ltsc2022"},
		{"Windows Server 2022 update", 10, 0, 25000, "ltsc2022"},
		{"Windows Server 2019", 10, 0, 17763, "ltsc2019"},
		{"Windows Server 2019 update", 10, 0, 19041, "ltsc2019"},
		{"Unknown old build", 10, 0, 15000, "ltsc2025"},
		{"Future build", 10, 0, 30000, "ltsc2025"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildToTag(tt.major, tt.minor, tt.build)
			if got != tt.want {
				t.Errorf("buildToTag(%d, %d, %d) = %q, want %q", tt.major, tt.minor, tt.build, got, tt.want)
			}
		})
	}
}

// --- defaultImage tests ---

func TestDefaultImage(t *testing.T) {
	img := defaultImage()
	if img == "" {
		t.Fatal("defaultImage() returned empty string")
	}

	switch runtime.GOOS {
	case "windows":
		if !strings.Contains(img, "servercore") {
			t.Errorf("defaultImage() = %q, expected servercore on Windows", img)
		}
		if !strings.Contains(img, "ltsc") {
			t.Errorf("defaultImage() = %q, expected ltsc tag", img)
		}
	default:
		if img != defaultImageLinux {
			t.Errorf("defaultImage() = %q, want %q", img, defaultImageLinux)
		}
	}
}

// --- New tests ---

func TestNew(t *testing.T) {
	rt, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if rt == nil {
		t.Fatal("New() returned nil")
	}
}

// --- Constants ---

func TestNamespaceConstant(t *testing.T) {
	if namespace != "ephemerd" {
		t.Errorf("namespace = %q, want %q", namespace, "ephemerd")
	}
}

func TestDefaultImageLinuxConstant(t *testing.T) {
	if defaultImageLinux == "" {
		t.Error("defaultImageLinux should not be empty")
	}
	if !strings.Contains(defaultImageLinux, "actions-runner") {
		t.Errorf("defaultImageLinux = %q, expected to contain 'actions-runner'", defaultImageLinux)
	}
}
