// Package vm manages virtual machines for cross-OS job execution.
//
// ephemerd uses VMs in two scenarios:
//
//  1. Linux VM (long-running): On Windows and macOS hosts, a lightweight Linux VM
//     runs containerd for Linux jobs. Same OCI images as native Linux.
//     - Windows: Hyper-V Gen 2 VM with direct kernel boot via HCS API
//     - macOS: Virtualization.framework Linux VM
//
//  2. macOS VM (per-job): On macOS hosts, ephemeral macOS VMs run macOS-native
//     jobs (Xcode, Swift, etc.). Each job gets a clone-on-write copy of a base
//     image that is destroyed after the job completes.
//
// Platform-specific implementations are in *_darwin.go, *_windows.go, and *_linux.go.
package vm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"strings"

	"github.com/containerd/containerd/v2/client"
	"golang.org/x/crypto/ssh"
)

// LinuxVMConfig configures the long-running Linux VM for Linux jobs on non-Linux hosts.
type LinuxVMConfig struct {
	// DataDir is the ephemerd data directory. VM assets live under <DataDir>/vm/linux/.
	DataDir string

	// CPUs is the number of virtual CPUs. Defaults to 2.
	CPUs uint

	// MemoryMB is the VM memory in megabytes. Defaults to 2048.
	MemoryMB uint64

	// DiskSizeGB is the VM root disk size in gigabytes (sparse). Defaults to 50.
	DiskSizeGB uint64

	// ContainerdPort is the port containerd listens on inside the VM. Defaults to 10000.
	ContainerdPort uint32

	// DindEnabled passes --dind to the VM's ephemerd serve, mounting a fake
	// Docker socket into each container.
	DindEnabled bool

	// DindAllowPrivileged forwards the host's dind.allow_privileged setting
	// to the in-VM ephemerd via the kernel cmdline. Without this, the in-VM
	// daemon reads its own (minimal) config and Linux defaults to false,
	// rejecting `docker run --privileged` siblings even when the host
	// operator explicitly opted in.
	DindAllowPrivileged bool

	Log *slog.Logger
}

// SetDefaults applies default values for unconfigured fields.
func (c *LinuxVMConfig) SetDefaults() {
	if c.CPUs == 0 {
		c.CPUs = 1
	}
	if c.MemoryMB == 0 {
		c.MemoryMB = 4096
	}
	if c.DiskSizeGB == 0 {
		c.DiskSizeGB = 50
	}
	if c.ContainerdPort == 0 {
		c.ContainerdPort = 10000
	}
}

// LinuxVM is a long-running Linux VM that hosts containerd for Linux jobs.
// Implemented per-platform: Virtualization.framework on macOS, Hyper-V on Windows.
type LinuxVM interface {
	// Client returns a containerd client connected to containerd inside the VM.
	Client() *client.Client

	// DispatchAddr returns the address of the dispatch gRPC server running
	// inside the VM (e.g. "localhost:10001"). Empty if dispatch is unavailable.
	DispatchAddr() string

	// Stop gracefully shuts down the VM.
	Stop()
}

// MacOSVMConfig configures per-job macOS VMs (macOS hosts only).
type MacOSVMConfig struct {
	// DataDir is the ephemerd data directory. VM assets live under <DataDir>/vm/macos/.
	DataDir string

	// DiskImage is the path to the installed macOS disk (produced from
	// an Apple IPSW via EnsureMacOSBaseImage). Each job gets an APFS
	// clone of this file. Not to be confused with the OCI base image
	// that jobs overlay onto the VM at runtime.
	DiskImage string

	// SSHSigner is the ephemeral SSH private key for guest access.
	// Generated fresh on each ephemerd startup — never persisted to disk.
	SSHSigner interface{} // crypto.Signer (ed25519.PrivateKey)

	// SSHPubKey is the authorized_keys-format public key to inject into
	// each job's virtio-fs share. The guest picks it up on boot.
	SSHPubKey string

	// CPUs per macOS VM. Defaults to 4.
	CPUs uint

	// MemoryMB per macOS VM. Defaults to 8192.
	MemoryMB uint64

	Log *slog.Logger
}

// SetDefaults applies default values for unconfigured fields.
func (c *MacOSVMConfig) SetDefaults() {
	if c.CPUs == 0 {
		c.CPUs = 2
	}
	if c.MemoryMB == 0 {
		c.MemoryMB = 2048
	}
}

// MacOSVM is an ephemeral macOS VM for a single job.
// Only available on macOS hosts via Virtualization.framework.
type MacOSVM interface {
	// WriteJITConfig writes the encoded JIT runner config to the job's shared
	// directory so the guest can pick it up on boot via virtio-fs.
	WriteJITConfig(encodedJIT string) error

	// Start boots the VM from a clone-on-write copy of the base image.
	Start(ctx context.Context) error

	// WaitForRunner blocks until the GitHub runner inside the VM is reachable.
	// Checks for a .ready sentinel file on the virtio-fs share first, then
	// falls back to SSH port 22. Returns the VM's discovered IP address.
	WaitForRunner(ctx context.Context) (string, error)

	// RunnerAddress returns the VM's discovered IP address, or empty if not yet known.
	RunnerAddress() string

	// Wait blocks until the VM exits. Returns the exit code.
	Wait(ctx context.Context) (int, error)

	// Stop forcefully stops the VM and deletes the clone.
	Stop()
}

// GenerateEphemeralSSHKey creates an in-memory ed25519 key pair for SSH
// access to macOS VMs. The private key is never written to disk — it lives
// only for the lifetime of this ephemerd process and rotates on restart.
// Returns the private key (as crypto.Signer) and the public key in
// authorized_keys format.
func GenerateEphemeralSSHKey() (ed25519.PrivateKey, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " ephemerd\n"
	return priv, pubLine, nil
}

// normalizeMAC converts a MAC address to a canonical lowercase form with
// zero-padded octets (e.g., "a:b:c:d:e:f" → "0a:0b:0c:0d:0e:0f").
// macOS arp output may omit leading zeros while Vz reports them — this
// ensures reliable comparison regardless of format.
func normalizeMAC(mac string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(mac)), ":")
	if len(parts) != 6 {
		return strings.ToLower(mac)
	}
	for i, p := range parts {
		if len(p) == 1 {
			parts[i] = "0" + p
		}
	}
	return strings.Join(parts, ":")
}
