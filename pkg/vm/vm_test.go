package vm

import (
	"runtime"
	"testing"
)

// --- LinuxVMConfig.SetDefaults ---

func TestLinuxVMConfig_SetDefaults(t *testing.T) {
	cfg := LinuxVMConfig{}
	cfg.SetDefaults()

	if cfg.CPUs != 1 {
		t.Errorf("CPUs = %d, want 1", cfg.CPUs)
	}
	if cfg.MemoryMB != 4096 {
		t.Errorf("MemoryMB = %d, want 4096", cfg.MemoryMB)
	}
	if cfg.DiskSizeGB != 50 {
		t.Errorf("DiskSizeGB = %d, want 50", cfg.DiskSizeGB)
	}
	if cfg.ContainerdPort != 10000 {
		t.Errorf("ContainerdPort = %d, want 10000", cfg.ContainerdPort)
	}
}

func TestLinuxVMConfig_SetDefaults_PreservesCustom(t *testing.T) {
	cfg := LinuxVMConfig{
		CPUs:           8,
		MemoryMB:       16384,
		DiskSizeGB:     200,
		ContainerdPort: 20000,
	}
	cfg.SetDefaults()

	if cfg.CPUs != 8 {
		t.Errorf("CPUs = %d, want 8", cfg.CPUs)
	}
	if cfg.MemoryMB != 16384 {
		t.Errorf("MemoryMB = %d, want 16384", cfg.MemoryMB)
	}
	if cfg.DiskSizeGB != 200 {
		t.Errorf("DiskSizeGB = %d, want 200", cfg.DiskSizeGB)
	}
	if cfg.ContainerdPort != 20000 {
		t.Errorf("ContainerdPort = %d, want 20000", cfg.ContainerdPort)
	}
}

func TestLinuxVMConfig_SetDefaults_PartialCustom(t *testing.T) {
	cfg := LinuxVMConfig{CPUs: 4}
	cfg.SetDefaults()

	if cfg.CPUs != 4 {
		t.Errorf("CPUs = %d, want 4 (custom)", cfg.CPUs)
	}
	if cfg.MemoryMB != 4096 {
		t.Errorf("MemoryMB = %d, want 4096 (default)", cfg.MemoryMB)
	}
}

// --- MacOSVMConfig.SetDefaults ---

func TestMacOSVMConfig_SetDefaults(t *testing.T) {
	cfg := MacOSVMConfig{}
	cfg.SetDefaults()

	if cfg.CPUs != 2 {
		t.Errorf("CPUs = %d, want 2", cfg.CPUs)
	}
	if cfg.MemoryMB != 2048 {
		t.Errorf("MemoryMB = %d, want 2048", cfg.MemoryMB)
	}
}

func TestMacOSVMConfig_SetDefaults_PreservesCustom(t *testing.T) {
	cfg := MacOSVMConfig{CPUs: 8, MemoryMB: 32768}
	cfg.SetDefaults()

	if cfg.CPUs != 8 {
		t.Errorf("CPUs = %d, want 8", cfg.CPUs)
	}
	if cfg.MemoryMB != 32768 {
		t.Errorf("MemoryMB = %d, want 32768", cfg.MemoryMB)
	}
}

// --- Stub behavior tests ---

func TestNewMacOSVM_StubOnNonDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("real implementation on darwin")
	}
	_, err := NewMacOSVM(MacOSVMConfig{}, "test-job")
	if err == nil {
		t.Error("expected error on non-darwin platform")
	}
}

// --- normalizeMAC ---

func TestNormalizeMAC(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Already canonical
		{"0a:0b:0c:0d:0e:0f", "0a:0b:0c:0d:0e:0f"},
		// Missing leading zeros (macOS arp output format)
		{"a:b:c:d:e:f", "0a:0b:0c:0d:0e:0f"},
		// Mixed
		{"aa:b:cc:0d:e:ff", "aa:0b:cc:0d:0e:ff"},
		// Uppercase
		{"AA:BB:CC:DD:EE:FF", "aa:bb:cc:dd:ee:ff"},
		// Mixed case with missing zeros
		{"A:B:C:D:E:F", "0a:0b:0c:0d:0e:0f"},
		// Whitespace
		{"  aa:bb:cc:dd:ee:ff  ", "aa:bb:cc:dd:ee:ff"},
		// Non-MAC input (passthrough)
		{"not-a-mac", "not-a-mac"},
		{"", ""},
		// Wrong number of octets
		{"aa:bb:cc", "aa:bb:cc"},
	}

	for _, tt := range tests {
		got := normalizeMAC(tt.input)
		if got != tt.want {
			t.Errorf("normalizeMAC(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- GenerateEphemeralSSHKey ---

func TestGenerateEphemeralSSHKey_ReturnsValidKey(t *testing.T) {
	priv, pubLine, err := GenerateEphemeralSSHKey()
	if err != nil {
		t.Fatalf("GenerateEphemeralSSHKey() error: %v", err)
	}
	if priv == nil {
		t.Fatal("private key is nil")
	}
	if len(priv) != 64 { // ed25519 private key is 64 bytes
		t.Errorf("private key length = %d, want 64", len(priv))
	}
	if pubLine == "" {
		t.Fatal("public key line is empty")
	}
}

func TestGenerateEphemeralSSHKey_PubKeyFormat(t *testing.T) {
	_, pubLine, err := GenerateEphemeralSSHKey()
	if err != nil {
		t.Fatalf("GenerateEphemeralSSHKey() error: %v", err)
	}
	// Should start with ssh-ed25519 and end with "ephemerd\n"
	if len(pubLine) < 20 {
		t.Fatalf("public key line too short: %q", pubLine)
	}
	if pubLine[:len("ssh-ed25519 ")] != "ssh-ed25519 " {
		t.Errorf("public key should start with 'ssh-ed25519 ', got %q", pubLine[:20])
	}
	if pubLine[len(pubLine)-9:] != "ephemerd\n" {
		t.Errorf("public key should end with 'ephemerd\\n', got %q", pubLine[len(pubLine)-9:])
	}
}

func TestGenerateEphemeralSSHKey_Uniqueness(t *testing.T) {
	_, pub1, err := GenerateEphemeralSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	_, pub2, err := GenerateEphemeralSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	if pub1 == pub2 {
		t.Error("two calls should produce different keys")
	}
}

func TestStartLinuxVM_ErrorsOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("stub test only applies on Linux")
	}
	_, err := StartLinuxVM(LinuxVMConfig{})
	if err == nil {
		t.Error("expected error on Linux (containerd runs natively)")
	}
}
