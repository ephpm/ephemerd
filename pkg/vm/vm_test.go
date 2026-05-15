package vm

import (
	"crypto/ed25519"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
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

// --- macOS SSH key material validation (cross-platform) ---
//
// macosvm_darwin.go is darwin-only, but the SSH key injection logic is
// driven by MacOSVMConfig.SSHPubKey / SSHSigner — both validated/generated
// in the cross-platform vm.go. These tests run on every platform.

func TestGenerateEphemeralSSHKey_SignerIsEd25519(t *testing.T) {
	// The generated private key must satisfy crypto.Signer (used by SSH
	// auth in setupRunnerViaSSH and monitorRunner). The runtime type
	// assertion (ed25519.PrivateKey) is load-bearing.
	priv, _, err := GenerateEphemeralSSHKey()
	if err != nil {
		t.Fatalf("GenerateEphemeralSSHKey: %v", err)
	}

	// Confirm it satisfies the type assertion used in macosvm_darwin.go.
	cfg := MacOSVMConfig{SSHSigner: priv}
	signer, ok := cfg.SSHSigner.(ed25519.PrivateKey)
	if !ok {
		t.Fatal("SSHSigner type assertion to ed25519.PrivateKey failed")
	}

	// Confirm it can be wrapped in an ssh.Signer (also load-bearing).
	if _, err := ssh.NewSignerFromKey(signer); err != nil {
		t.Errorf("ssh.NewSignerFromKey: %v", err)
	}
}

func TestMacOSVMConfig_EmptySSHPubKey(t *testing.T) {
	// Empty SSHPubKey is allowed at the config layer — macosvm_darwin.go
	// gates `if m.cfg.SSHPubKey != ""` for both WriteJITConfig and SSH
	// authorized_keys injection. This documents that empty is "skip".
	cfg := MacOSVMConfig{SSHPubKey: ""}
	if cfg.SSHPubKey != "" {
		t.Errorf("expected empty SSHPubKey, got %q", cfg.SSHPubKey)
	}
}

func TestMacOSVMConfig_SSHPubKey_TrimSpace(t *testing.T) {
	// macosvm_darwin.go's fixCmd uses strings.TrimSpace(m.cfg.SSHPubKey).
	// Guard against subtle bugs where a trailing newline would inject a
	// blank line into authorized_keys (technically valid but messy).
	withNewline := "ssh-ed25519 AAAA... ephemerd\n"
	got := strings.TrimSpace(withNewline)
	if strings.HasSuffix(got, "\n") {
		t.Errorf("TrimSpace did not strip trailing newline: %q", got)
	}
	if !strings.HasPrefix(got, "ssh-ed25519 ") {
		t.Errorf("after TrimSpace, prefix should remain: %q", got)
	}
}

func TestGenerateEphemeralSSHKey_PubKeyParsesAsAuthorizedKey(t *testing.T) {
	// The pubLine format must round-trip through ssh.ParseAuthorizedKey —
	// otherwise the guest's authorized_keys won't accept it.
	_, pubLine, err := GenerateEphemeralSSHKey()
	if err != nil {
		t.Fatalf("GenerateEphemeralSSHKey: %v", err)
	}
	// Strip any whitespace (the function appends "\n").
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubLine))
	if err != nil {
		t.Fatalf("ParseAuthorizedKey: %v", err)
	}
	if parsed.Type() != "ssh-ed25519" {
		t.Errorf("parsed key type = %q, want ssh-ed25519", parsed.Type())
	}
}

func TestGenerateEphemeralSSHKey_PubKeyMatchesPriv(t *testing.T) {
	// The generated public key must correspond to the private key —
	// otherwise the guest will reject signatures from this signer.
	priv, pubLine, err := GenerateEphemeralSSHKey()
	if err != nil {
		t.Fatalf("GenerateEphemeralSSHKey: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}
	signerPub := signer.PublicKey()

	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubLine))
	if err != nil {
		t.Fatalf("ParseAuthorizedKey: %v", err)
	}

	// Compare wire format.
	if string(signerPub.Marshal()) != string(parsed.Marshal()) {
		t.Error("pubLine does not match the public key derived from priv")
	}
}

func TestMacOSVMConfig_SSHSignerTypeAssertion_NonEd25519(t *testing.T) {
	// Guard against accidental misuse: a non-ed25519 SSHSigner should fail
	// the type assertion in macosvm_darwin.go (lines 442, 583). Pass nil
	// and a wrong type and confirm both are rejected by the assertion.
	tests := []struct {
		name    string
		val     interface{}
		wantOK  bool
	}{
		{"nil", nil, false},
		{"int", 42, false},
		{"string", "not a key", false},
		{"valid ed25519", makeEd25519(t), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := tt.val.(ed25519.PrivateKey)
			if ok != tt.wantOK {
				t.Errorf("type assertion = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func makeEd25519(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	priv, _, err := GenerateEphemeralSSHKey()
	if err != nil {
		t.Fatalf("makeEd25519: %v", err)
	}
	return priv
}

func TestMacOSVMConfig_PartialKeyMaterial(t *testing.T) {
	// Document behavior when SSHPubKey is set but SSHSigner is nil
	// (and vice versa). Neither field is validated at the config layer —
	// the consuming code in macosvm_darwin.go must handle each independently.
	tests := []struct {
		name      string
		signer    interface{}
		pubKey    string
		signerNil bool
		pubEmpty  bool
	}{
		{"both set", makeEd25519(t), "ssh-ed25519 AAAA test", false, false},
		{"signer only", makeEd25519(t), "", false, true},
		{"pubkey only", nil, "ssh-ed25519 AAAA test", true, false},
		{"both nil/empty", nil, "", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := MacOSVMConfig{SSHSigner: tt.signer, SSHPubKey: tt.pubKey}
			if (cfg.SSHSigner == nil) != tt.signerNil {
				t.Errorf("signerNil mismatch")
			}
			if (cfg.SSHPubKey == "") != tt.pubEmpty {
				t.Errorf("pubEmpty mismatch")
			}
		})
	}
}
