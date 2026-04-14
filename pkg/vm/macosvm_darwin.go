//go:build darwin

package vm

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// darwinMacOSVM is an ephemeral macOS VM for a single job on macOS hosts.
// Uses Virtualization.framework with clone-on-write disk copies.
type darwinMacOSVM struct {
	cfg       MacOSVMConfig
	id        string
	vm        *vz.VirtualMachine
	clonePath string
	jobDir    string // shared directory for JIT config exchange
	macAddr   string // VM's MAC address for ARP-based IP discovery
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewMacOSVM creates a new per-job macOS VM. Call Start() to boot it.
func NewMacOSVM(cfg MacOSVMConfig, jobID string) (MacOSVM, error) {
	cfg.SetDefaults()

	if cfg.BaseImage == "" {
		return nil, fmt.Errorf("vm.macos.base_image is required for macOS-native jobs")
	}
	if _, err := os.Stat(cfg.BaseImage); os.IsNotExist(err) {
		return nil, fmt.Errorf("macOS base image not found: %s\n\nRun 'ephemerd vm setup-macos' to create one", cfg.BaseImage)
	}

	jobDir := filepath.Join(cfg.DataDir, "vm", "macos", "jobs", jobID)

	return &darwinMacOSVM{
		cfg:    cfg,
		id:     jobID,
		jobDir: jobDir,
		done:   make(chan struct{}),
	}, nil
}

// WriteJITConfig writes the encoded JIT runner config to the job's shared
// directory so the macOS guest can pick it up on boot.
func (m *darwinMacOSVM) WriteJITConfig(encodedJIT string) error {
	if err := os.MkdirAll(m.jobDir, 0o755); err != nil {
		return fmt.Errorf("creating job directory: %w", err)
	}
	jitPath := filepath.Join(m.jobDir, ".jit_config")
	if err := os.WriteFile(jitPath, []byte(encodedJIT), 0o600); err != nil {
		return fmt.Errorf("writing JIT config: %w", err)
	}
	return nil
}

func (m *darwinMacOSVM) Start(ctx context.Context) error {
	// Clone-on-write copy of the base image for this job.
	// APFS clones are near-instant — no data is copied until writes occur.
	cloneDir := filepath.Join(m.cfg.DataDir, "vm", "macos", "jobs")
	if err := os.MkdirAll(cloneDir, 0o755); err != nil {
		return fmt.Errorf("creating clone directory: %w", err)
	}

	m.clonePath = filepath.Join(cloneDir, m.id+".img")

	// Use APFS clone (cp -c) for instant copy-on-write
	if err := exec.CommandContext(ctx, "cp", "-c", m.cfg.BaseImage, m.clonePath).Run(); err != nil {
		// Fall back to regular copy if APFS clone isn't supported
		m.cfg.Log.Warn("APFS clone failed, falling back to regular copy", "error", err)
		if err := exec.CommandContext(ctx, "cp", m.cfg.BaseImage, m.clonePath).Run(); err != nil {
			return fmt.Errorf("copying base image: %w", err)
		}
	}

	// Ensure job shared directory exists for JIT config exchange
	if err := os.MkdirAll(m.jobDir, 0o755); err != nil {
		return fmt.Errorf("creating job shared directory: %w", err)
	}

	bootCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	// macOS guests use the platform-specific boot loader (not Linux boot loader)
	bootLoader, err := vz.NewMacOSBootLoader()
	if err != nil {
		cancel()
		return fmt.Errorf("creating macOS boot loader: %w", err)
	}

	vmConfig, err := vz.NewVirtualMachineConfiguration(bootLoader, m.cfg.CPUs, m.cfg.MemoryMB*1024*1024)
	if err != nil {
		cancel()
		return fmt.Errorf("creating VM config: %w", err)
	}

	// macOS platform config (required for macOS guests on Apple Silicon)
	platformConfig, err := vz.NewMacPlatformConfiguration()
	if err != nil {
		cancel()
		return fmt.Errorf("creating platform config: %w", err)
	}
	vmConfig.SetPlatformVirtualMachineConfiguration(platformConfig)

	// Graphics device (macOS guest requires it even headless)
	graphicsConfig, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		cancel()
		return fmt.Errorf("creating graphics config: %w", err)
	}
	display, err := vz.NewMacGraphicsDisplayConfiguration(1920, 1200, 80)
	if err != nil {
		cancel()
		return fmt.Errorf("creating display config: %w", err)
	}
	graphicsConfig.SetDisplays(display)
	vmConfig.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{graphicsConfig})

	// Entropy
	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		cancel()
		return fmt.Errorf("creating entropy device: %w", err)
	}
	vmConfig.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	// NAT networking
	natAttachment, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		cancel()
		return fmt.Errorf("creating NAT attachment: %w", err)
	}
	netConfig, err := vz.NewVirtioNetworkDeviceConfiguration(natAttachment)
	if err != nil {
		cancel()
		return fmt.Errorf("creating network config: %w", err)
	}
	macAddress, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		cancel()
		return fmt.Errorf("creating MAC address: %w", err)
	}
	netConfig.SetMACAddress(macAddress)
	m.macAddr = macAddress.String()
	vmConfig.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netConfig})

	// Disk: the cloned image
	diskAttachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
		m.clonePath, false, vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFsync,
	)
	if err != nil {
		cancel()
		return fmt.Errorf("creating disk attachment: %w", err)
	}
	blockDevice, err := vz.NewVirtioBlockDeviceConfiguration(diskAttachment)
	if err != nil {
		cancel()
		return fmt.Errorf("creating block device: %w", err)
	}
	vmConfig.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blockDevice})

	// Shared directory: job data dir → /Volumes/ephemerd in guest
	// The guest reads .jit_config from this share to start the GitHub runner.
	shareDir, err := vz.NewSharedDirectory(m.jobDir, false)
	if err != nil {
		cancel()
		return fmt.Errorf("creating shared directory: %w", err)
	}
	singleShare, err := vz.NewSingleDirectoryShare(shareDir)
	if err != nil {
		cancel()
		return fmt.Errorf("creating directory share: %w", err)
	}
	fsConfig, err := vz.NewVirtioFileSystemDeviceConfiguration("ephemerd")
	if err != nil {
		cancel()
		return fmt.Errorf("creating filesystem device: %w", err)
	}
	fsConfig.SetDirectoryShare(singleShare)
	vmConfig.SetDirectorySharingDevicesVirtualMachineConfiguration([]vz.DirectorySharingDeviceConfiguration{fsConfig})

	ok, err := vmConfig.Validate()
	if err != nil || !ok {
		cancel()
		return fmt.Errorf("VM config validation failed: %w", err)
	}

	vm, err := vz.NewVirtualMachine(vmConfig)
	if err != nil {
		cancel()
		return fmt.Errorf("creating VM: %w", err)
	}
	m.vm = vm

	m.cfg.Log.Info("booting macOS VM", "id", m.id, "cpus", m.cfg.CPUs, "memory_mb", m.cfg.MemoryMB, "mac", m.macAddr)

	if err := vm.Start(); err != nil {
		cancel()
		return fmt.Errorf("starting macOS VM: %w", err)
	}

	go func() {
		defer close(m.done)
		for {
			select {
			case <-bootCtx.Done():
				return
			case <-time.After(1 * time.Second):
				state := vm.State()
				if state == vz.VirtualMachineStateStopped || state == vz.VirtualMachineStateError {
					return
				}
			}
		}
	}()

	return nil
}

func (m *darwinMacOSVM) RunnerAddress() string {
	ip, err := m.discoverIP()
	if err != nil {
		m.cfg.Log.Warn("failed to discover macOS VM IP", "id", m.id, "error", err)
		return ""
	}
	return ip
}

// WaitForRunner polls until the GitHub Actions runner inside the VM signals
// readiness by writing a .ready file to the virtio-fs shared directory.
// Falls back to IP discovery + SSH check if the guest doesn't write the file.
// Returns the VM's IP address.
func (m *darwinMacOSVM) WaitForRunner(ctx context.Context) (string, error) {
	m.cfg.Log.Info("waiting for macOS VM runner to become reachable", "id", m.id)

	readyPath := filepath.Join(m.jobDir, ".ready")

	for i := range 120 { // up to ~2 minutes
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-m.done:
			return "", fmt.Errorf("macOS VM exited before runner became reachable")
		case <-time.After(1 * time.Second):
		}

		// Primary check: guest writes .ready file to the shared directory
		// after the runner starts. This works regardless of SSH being enabled.
		if _, err := os.Stat(readyPath); err == nil {
			m.cfg.Log.Info("macOS VM runner signaled ready via .ready file", "id", m.id)
			ip, err := m.discoverIP()
			if err != nil {
				m.cfg.Log.Warn("runner ready but IP not yet discoverable, continuing to poll", "id", m.id, "error", err)
				continue
			}
			m.cfg.Log.Info("macOS VM runner reachable", "id", m.id, "ip", ip)
			return ip, nil
		}

		// Ping the NAT subnet to populate ARP entries. Vz NAT uses
		// 192.168.64.0/24 by default. Without this, the ARP table may
		// not have the VM's entry until the guest sends outbound traffic.
		if i%5 == 0 {
			m.probeSubnet()
		}

		ip, err := m.discoverIP()
		if err != nil {
			if i%15 == 0 && i > 0 {
				m.cfg.Log.Debug("still waiting for macOS VM IP", "id", m.id, "attempt", i)
			}
			continue
		}

		// Fallback: check if SSH is reachable as a proxy for the VM being booted.
		// This covers base images that don't write .ready but have SSH enabled.
		conn, err := net.DialTimeout("tcp", ip+":22", 2*time.Second)
		if err != nil {
			if i%15 == 0 && i > 0 {
				m.cfg.Log.Debug("VM has IP but SSH not ready yet", "id", m.id, "ip", ip, "attempt", i)
			}
			continue
		}
		conn.Close()

		m.cfg.Log.Info("macOS VM runner reachable (SSH fallback)", "id", m.id, "ip", ip)
		return ip, nil
	}

	return "", fmt.Errorf("timed out waiting for macOS VM runner (id=%s)", m.id)
}

// probeSubnet sends ICMP pings across the Vz NAT subnet to populate
// the host's ARP table. Without this, a quiet VM won't appear in ARP
// until it sends outbound traffic on its own.
func (m *darwinMacOSVM) probeSubnet() {
	// Vz NAT typically uses 192.168.64.0/24. Ping a small range around
	// the common DHCP allocation window (.2 through .10).
	for i := 2; i <= 10; i++ {
		ip := fmt.Sprintf("192.168.64.%d", i)
		// Non-blocking ping: just send and move on. We only care about
		// triggering ARP, not about the reply.
		go func(addr string) {
			cmd := exec.Command("ping", "-c", "1", "-W", "100", addr)
			cmd.Run() // ignore errors — host may not respond
		}(ip)
	}
}

// discoverIP finds the VM's IP by looking up its MAC address in the ARP table.
func (m *darwinMacOSVM) discoverIP() (string, error) {
	if m.macAddr == "" {
		return "", fmt.Errorf("no MAC address recorded for VM")
	}

	// Parse ARP table: arp -an outputs lines like:
	//   ? (192.168.64.2) at aa:bb:cc:dd:ee:ff on bridge100 ifscope [ethernet]
	out, err := exec.Command("arp", "-an").Output()
	if err != nil {
		return "", fmt.Errorf("running arp: %w", err)
	}

	targetMAC := normalizeMAC(m.macAddr)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		// Extract MAC from ARP line and normalize both for comparison.
		// macOS arp may omit leading zeros (e.g., "a:b:c:d:e:f" vs "0a:0b:0c:0d:0e:0f").
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] == "(incomplete)" {
			continue
		}
		arpMAC := normalizeMAC(fields[3])
		if arpMAC != targetMAC {
			continue
		}
		// Extract IP from between parentheses
		start := strings.Index(line, "(")
		end := strings.Index(line, ")")
		if start >= 0 && end > start {
			return line[start+1 : end], nil
		}
	}

	return "", fmt.Errorf("MAC %s not found in ARP table", m.macAddr)
}


func (m *darwinMacOSVM) Wait(ctx context.Context) (int, error) {
	select {
	case <-m.done:
		return 0, nil
	case <-ctx.Done():
		return 1, ctx.Err()
	}
}

func (m *darwinMacOSVM) Stop() {
	m.cfg.Log.Info("stopping macOS VM", "id", m.id)

	if m.vm != nil {
		if m.vm.CanRequestStop() {
			m.vm.RequestStop()
		}

		select {
		case <-m.done:
		case <-time.After(15 * time.Second):
			m.cfg.Log.Warn("macOS VM did not stop gracefully, forcing", "id", m.id)
			m.vm.Stop()
		}
	}

	if m.cancel != nil {
		m.cancel()
	}

	// Delete the clone
	if m.clonePath != "" {
		if err := os.Remove(m.clonePath); err != nil && !os.IsNotExist(err) {
			m.cfg.Log.Warn("failed to remove VM clone", "path", m.clonePath, "error", err)
		}
	}

	// Clean up the job shared directory
	if m.jobDir != "" {
		if err := os.RemoveAll(m.jobDir); err != nil {
			m.cfg.Log.Warn("failed to remove job directory", "path", m.jobDir, "error", err)
		}
	}

	m.cfg.Log.Info("macOS VM destroyed", "id", m.id)
}
