package runtime

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/runtime-spec/specs-go"
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

// --- defaultImage tests ---

func TestDefaultImage_NonEmpty(t *testing.T) {
	img := defaultImage()
	if img == "" {
		t.Fatal("defaultImage() returned empty string")
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

// --- seccompOpts tests ---

func TestSeccompOpts(t *testing.T) {
	opts := seccompOpts()
	switch runtime.GOOS {
	case "linux":
		if len(opts) == 0 {
			t.Error("seccompOpts() on Linux should return profile options")
		}
	default:
		if opts != nil {
			t.Errorf("seccompOpts() on %s should return nil, got %v", runtime.GOOS, opts)
		}
	}
}

// --- withDockerSocket tests ---

func TestWithDockerSocket(t *testing.T) {
	s := &ocispec.Spec{}
	opt := withDockerSocket("/tmp/docker.sock")
	if err := opt(nil, nil, nil, s); err != nil {
		t.Fatalf("withDockerSocket error: %v", err)
	}

	if len(s.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(s.Mounts))
	}
	m := s.Mounts[0]
	if m.Destination != "/var/run/docker.sock" {
		t.Errorf("Destination = %q, want /var/run/docker.sock", m.Destination)
	}
	if m.Source != "/tmp/docker.sock" {
		t.Errorf("Source = %q, want /tmp/docker.sock", m.Source)
	}
	if m.Type != "bind" {
		t.Errorf("Type = %q, want bind", m.Type)
	}
	hasRW := false
	for _, opt := range m.Options {
		if opt == "rw" {
			hasRW = true
		}
	}
	if !hasRW {
		t.Error("expected 'rw' in mount options")
	}
}

func TestWithDockerSocket_NilMounts(t *testing.T) {
	s := &ocispec.Spec{Mounts: nil}
	opt := withDockerSocket("/sock")
	if err := opt(nil, nil, nil, s); err != nil {
		t.Fatal(err)
	}
	if len(s.Mounts) != 1 {
		t.Errorf("expected 1 mount, got %d", len(s.Mounts))
	}
}

// --- withRunnerMount tests ---

func TestWithRunnerMount(t *testing.T) {
	s := &ocispec.Spec{}
	opt := withRunnerMount("/host/runner", "/actions-runner")
	if err := opt(nil, nil, nil, s); err != nil {
		t.Fatalf("withRunnerMount error: %v", err)
	}

	if len(s.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(s.Mounts))
	}
	m := s.Mounts[0]
	if m.Destination != "/actions-runner" {
		t.Errorf("Destination = %q, want /actions-runner", m.Destination)
	}
	if m.Source != "/host/runner" {
		t.Errorf("Source = %q, want /host/runner", m.Source)
	}

	if runtime.GOOS == "windows" {
		// On Windows, type is empty (mapped directories)
		hasRW := false
		for _, opt := range m.Options {
			if opt == "rw" {
				hasRW = true
			}
		}
		if !hasRW {
			t.Error("expected 'rw' in mount options on Windows")
		}
	} else {
		if m.Type != "bind" {
			t.Errorf("Type = %q, want bind", m.Type)
		}
	}
}

// --- withHyperVIsolation tests ---

func TestWithHyperVIsolation(t *testing.T) {
	s := &ocispec.Spec{}
	opt := withHyperVIsolation()
	if err := opt(nil, nil, nil, s); err != nil {
		t.Fatalf("withHyperVIsolation error: %v", err)
	}

	if s.Windows == nil {
		t.Fatal("Windows section should be set")
	}
	if s.Windows.HyperV == nil {
		t.Error("HyperV should be set")
	}
}

func TestWithHyperVIsolation_ExistingWindows(t *testing.T) {
	s := &ocispec.Spec{
		Windows: &ocispec.Windows{},
	}
	opt := withHyperVIsolation()
	if err := opt(nil, nil, nil, s); err != nil {
		t.Fatal(err)
	}
	if s.Windows.HyperV == nil {
		t.Error("HyperV should be set even with existing Windows section")
	}
}

// --- withWindowsNetwork tests ---

func TestWithWindowsNetwork(t *testing.T) {
	s := &ocispec.Spec{}
	opt := withWindowsNetwork("ns-123", "ep-456")
	if err := opt(nil, nil, nil, s); err != nil {
		t.Fatalf("withWindowsNetwork error: %v", err)
	}

	if s.Windows == nil {
		t.Fatal("Windows section should be set")
	}
	if s.Windows.Network == nil {
		t.Fatal("Network section should be set")
	}
	if s.Windows.Network.NetworkNamespace != "ns-123" {
		t.Errorf("NetworkNamespace = %q, want ns-123", s.Windows.Network.NetworkNamespace)
	}
	if len(s.Windows.Network.EndpointList) != 1 || s.Windows.Network.EndpointList[0] != "ep-456" {
		t.Errorf("EndpointList = %v, want [ep-456]", s.Windows.Network.EndpointList)
	}
}

func TestWithWindowsNetwork_AppendsEndpoint(t *testing.T) {
	s := &ocispec.Spec{
		Windows: &ocispec.Windows{
			Network: &ocispec.WindowsNetwork{
				EndpointList: []string{"existing-ep"},
			},
		},
	}
	opt := withWindowsNetwork("ns-123", "ep-456")
	if err := opt(nil, nil, nil, s); err != nil {
		t.Fatal(err)
	}
	if len(s.Windows.Network.EndpointList) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(s.Windows.Network.EndpointList))
	}
	if s.Windows.Network.EndpointList[0] != "existing-ep" {
		t.Errorf("first endpoint = %q, want existing-ep", s.Windows.Network.EndpointList[0])
	}
	if s.Windows.Network.EndpointList[1] != "ep-456" {
		t.Errorf("second endpoint = %q, want ep-456", s.Windows.Network.EndpointList[1])
	}
}

// --- withDNSMount tests ---

func TestWithDNSMount(t *testing.T) {
	hostDir := t.TempDir()
	containerDir := hostDir // same on Linux/Windows

	s := &ocispec.Spec{}
	opt := withDNSMount(hostDir, containerDir, "test-job-1")
	if err := opt(nil, nil, nil, s); err != nil {
		t.Fatalf("withDNSMount error: %v", err)
	}

	// Should have created the resolv.conf file
	confPath := filepath.Join(hostDir, "dns", "test-job-1.conf")
	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("resolv.conf not created: %v", err)
	}
	if len(data) == 0 {
		t.Error("resolv.conf is empty")
	}
	if !strings.Contains(string(data), "nameserver") {
		t.Errorf("resolv.conf should contain nameserver, got: %q", string(data))
	}

	// Should have added a mount
	if len(s.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(s.Mounts))
	}
	m := s.Mounts[0]
	if m.Destination != "/etc/resolv.conf" {
		t.Errorf("Destination = %q, want /etc/resolv.conf", m.Destination)
	}
	expectedSrc := filepath.Join(containerDir, "dns", "test-job-1.conf")
	if m.Source != expectedSrc {
		t.Errorf("Source = %q, want %q", m.Source, expectedSrc)
	}
}

func TestWithDNSMount_DifferentContainerDir(t *testing.T) {
	hostDir := t.TempDir()
	containerDir := "/mnt/ephemerd" // virtio-fs path

	s := &ocispec.Spec{}
	opt := withDNSMount(hostDir, containerDir, "job-42")
	if err := opt(nil, nil, nil, s); err != nil {
		t.Fatal(err)
	}

	if len(s.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(s.Mounts))
	}
	// Source should use the container dir, not host dir
	wantSrc := filepath.Join("/mnt/ephemerd", "dns", "job-42.conf")
	if s.Mounts[0].Source != wantSrc {
		t.Errorf("Source = %q, want %q", s.Mounts[0].Source, wantSrc)
	}
}

// --- copyDirForJob tests ---

func TestCopyDirForJob(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyDirForJob(src, dst); err != nil {
		t.Fatalf("copyDirForJob error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "file.txt"))
	if err != nil {
		t.Fatalf("reading copied file: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("copied file = %q, want %q", string(data), "hello")
	}
}
