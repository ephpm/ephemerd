//go:build darwin

package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewMacOSVM creates a new per-job macOS VM. Call Start() to boot it.
func NewMacOSVM(cfg MacOSVMConfig, jobID string) (MacOSVM, error) {
	cfg.setDefaults()

	if cfg.BaseImage == "" {
		return nil, fmt.Errorf("vm.macos.base_image is required for macOS-native jobs")
	}
	if _, err := os.Stat(cfg.BaseImage); os.IsNotExist(err) {
		return nil, fmt.Errorf("macOS base image not found: %s\n\nRun 'ephemerd vm setup-macos' to create one", cfg.BaseImage)
	}

	return &darwinMacOSVM{
		cfg:  cfg,
		id:   jobID,
		done: make(chan struct{}),
	}, nil
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

	m.cfg.Log.Info("booting macOS VM", "id", m.id, "cpus", m.cfg.CPUs, "memory_mb", m.cfg.MemoryMB)

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
	// The macOS VM gets an IP via NAT. The GitHub runner inside the VM
	// listens after being configured via SSH or a startup script baked
	// into the base image.
	// TODO: discover the VM's IP via ARP or Bonjour/mDNS
	return fmt.Sprintf("ssh://ephemerd@%s-vm.local", m.id)
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
		if canStop, err := m.vm.CanRequestStop(); err == nil && canStop {
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

	m.cfg.Log.Info("macOS VM destroyed", "id", m.id)
}
