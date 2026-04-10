//go:build darwin

package vm

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/containerd/containerd/v2/client"
)

// darwinLinuxVM runs a Linux VM on macOS using Virtualization.framework.
type darwinLinuxVM struct {
	cfg    LinuxVMConfig
	vm     *vz.VirtualMachine
	client *client.Client
	cancel context.CancelFunc
	done   chan struct{}
}

// StartLinuxVM boots a Linux VM on macOS and waits for containerd inside it.
func StartLinuxVM(cfg LinuxVMConfig) (LinuxVM, error) {
	cfg.SetDefaults()

	l := &darwinLinuxVM{
		cfg:  cfg,
		done: make(chan struct{}),
	}

	if err := l.ensureAssets(); err != nil {
		return nil, fmt.Errorf("preparing VM assets: %w", err)
	}

	if err := l.boot(); err != nil {
		return nil, fmt.Errorf("booting Linux VM: %w", err)
	}

	if err := l.waitForContainerd(); err != nil {
		l.Stop()
		return nil, fmt.Errorf("containerd not ready in VM: %w", err)
	}

	return l, nil
}

func (l *darwinLinuxVM) Client() *client.Client {
	return l.client
}

func (l *darwinLinuxVM) DispatchAddr() string {
	return "" // macOS Linux VMs don't use the dispatch architecture
}

func (l *darwinLinuxVM) Stop() {
	l.cfg.Log.Info("stopping Linux VM")

	if l.client != nil {
		l.client.Close()
	}

	if l.vm != nil {
		if canStop, err := l.vm.CanRequestStop(); err == nil && canStop {
			if _, err := l.vm.RequestStop(); err != nil {
				l.cfg.Log.Warn("graceful VM stop failed, forcing", "error", err)
			}
		}

		select {
		case <-l.done:
		case <-time.After(10 * time.Second):
			l.cfg.Log.Warn("VM did not stop gracefully, forcing stop")
			if err := l.vm.Stop(); err != nil {
				l.cfg.Log.Error("failed to force-stop VM", "error", err)
			}
		}
	}

	if l.cancel != nil {
		l.cancel()
	}

	l.cfg.Log.Info("Linux VM stopped")
}

func (l *darwinLinuxVM) vmDir() string {
	return filepath.Join(l.cfg.DataDir, "vm", "linux")
}

func (l *darwinLinuxVM) kernelPath() string {
	return filepath.Join(l.vmDir(), "vmlinuz")
}

func (l *darwinLinuxVM) initrdPath() string {
	return filepath.Join(l.vmDir(), "initrd")
}

func (l *darwinLinuxVM) diskPath() string {
	return filepath.Join(l.vmDir(), "disk.img")
}

func (l *darwinLinuxVM) ensureAssets() error {
	dir := l.vmDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating vm directory: %w", err)
	}

	for _, path := range []string{l.kernelPath(), l.initrdPath()} {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return fmt.Errorf("required VM asset not found: %s\n\nRun 'ephemerd vm setup' to download the Linux kernel and initrd", path)
		}
	}

	// Create sparse disk image if it doesn't exist
	if _, err := os.Stat(l.diskPath()); os.IsNotExist(err) {
		l.cfg.Log.Info("creating VM disk image", "path", l.diskPath(), "size_gb", l.cfg.DiskSizeGB)
		f, err := os.Create(l.diskPath())
		if err != nil {
			return fmt.Errorf("creating disk image: %w", err)
		}
		if err := f.Truncate(int64(l.cfg.DiskSizeGB) * 1024 * 1024 * 1024); err != nil {
			f.Close()
			return fmt.Errorf("sizing disk image: %w", err)
		}
		f.Close()
	}

	return nil
}

func (l *darwinLinuxVM) boot() error {
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel

	bootLoader, err := vz.NewLinuxBootLoader(
		l.kernelPath(),
		vz.WithInitrd(l.initrdPath()),
		vz.WithCommandLine(fmt.Sprintf(
			"console=hvc0 root=/dev/vda rw ephemerd.containerd_port=%d ephemerd.share_tag=ephemerd quiet",
			l.cfg.ContainerdPort,
		)),
	)
	if err != nil {
		cancel()
		return fmt.Errorf("creating boot loader: %w", err)
	}

	vmConfig, err := vz.NewVirtualMachineConfiguration(bootLoader, l.cfg.CPUs, l.cfg.MemoryMB*1024*1024)
	if err != nil {
		cancel()
		return fmt.Errorf("creating VM config: %w", err)
	}

	// Entropy device
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

	// Disk
	diskAttachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
		l.diskPath(), false, vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFsync,
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

	// Shared directory: ephemerd data dir → /mnt/ephemerd in guest
	shareDir, err := vz.NewSharedDirectory(l.cfg.DataDir, false)
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
	l.vm = vm

	l.cfg.Log.Info("booting Linux VM", "cpus", l.cfg.CPUs, "memory_mb", l.cfg.MemoryMB, "disk_gb", l.cfg.DiskSizeGB)

	if err := vm.Start(); err != nil {
		cancel()
		return fmt.Errorf("starting VM: %w", err)
	}

	go func() {
		defer close(l.done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
				state := vm.State()
				if state == vz.VirtualMachineStateStopped || state == vz.VirtualMachineStateError {
					l.cfg.Log.Info("Linux VM exited", "state", state)
					return
				}
			}
		}
	}()

	return nil
}

func (l *darwinLinuxVM) waitForContainerd() error {
	addr := fmt.Sprintf("127.0.0.1:%d", l.cfg.ContainerdPort)
	l.cfg.Log.Info("waiting for containerd in Linux VM", "address", addr)

	for i := range 60 {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			if i%10 == 0 && i > 0 {
				l.cfg.Log.Debug("still waiting for containerd in VM", "attempt", i)
			}
			time.Sleep(1 * time.Second)
			continue
		}
		conn.Close()

		l.client, err = client.New(addr)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = l.client.Version(ctx)
		cancel()
		if err == nil {
			l.cfg.Log.Info("containerd ready in Linux VM")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("timed out waiting for containerd at %s", addr)
}
