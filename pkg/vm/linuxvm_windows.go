//go:build windows

package vm

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/v2/client"
)

// windowsLinuxVM runs a Linux VM on Windows using Hyper-V.
type windowsLinuxVM struct {
	cfg    LinuxVMConfig
	vmName string
	client *client.Client
}

// StartLinuxVM boots a Linux VM on Windows via Hyper-V and waits for containerd.
//
// Prerequisites:
// - Hyper-V must be enabled
// - A Linux VHDX image with containerd pre-installed must exist at <DataDir>/vm/linux/disk.vhdx
//   The VM init system starts containerd listening on 0.0.0.0:<ContainerdPort>
func StartLinuxVM(cfg LinuxVMConfig) (LinuxVM, error) {
	cfg.SetDefaults()

	l := &windowsLinuxVM{
		cfg:    cfg,
		vmName: "ephemerd-linux",
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

func (l *windowsLinuxVM) Client() *client.Client {
	return l.client
}

func (l *windowsLinuxVM) Stop() {
	l.cfg.Log.Info("stopping Linux VM")

	if l.client != nil {
		l.client.Close()
	}

	// Stop the Hyper-V VM
	if err := l.powershell("Stop-VM", "-Name", l.vmName, "-TurnOff", "-Force"); err != nil {
		l.cfg.Log.Warn("failed to stop VM", "error", err)
	}

	l.cfg.Log.Info("Linux VM stopped")
}

func (l *windowsLinuxVM) vmDir() string {
	return filepath.Join(l.cfg.DataDir, "vm", "linux")
}

func (l *windowsLinuxVM) diskPath() string {
	return filepath.Join(l.vmDir(), "disk.vhdx")
}

func (l *windowsLinuxVM) ensureAssets() error {
	dir := l.vmDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating vm directory: %w", err)
	}

	if _, err := os.Stat(l.diskPath()); os.IsNotExist(err) {
		return fmt.Errorf("required VM disk not found: %s\n\nProvide a Linux VHDX image with containerd pre-installed", l.diskPath())
	}

	return nil
}

func (l *windowsLinuxVM) boot() error {
	l.cfg.Log.Info("booting Linux VM via Hyper-V", "name", l.vmName, "cpus", l.cfg.CPUs, "memory_mb", l.cfg.MemoryMB)

	// Remove any existing VM with the same name (leftover from crash)
	_ = l.powershell("Remove-VM", "-Name", l.vmName, "-Force")

	// Create the VM
	if err := l.powershell(
		"New-VM",
		"-Name", l.vmName,
		"-MemoryStartupBytes", fmt.Sprintf("%dMB", l.cfg.MemoryMB),
		"-Generation", "2",
		"-VHDPath", l.diskPath(),
		"-SwitchName", "Default Switch",
	); err != nil {
		return fmt.Errorf("creating VM: %w", err)
	}

	// Set CPU count
	if err := l.powershell(
		"Set-VMProcessor",
		"-VMName", l.vmName,
		"-Count", fmt.Sprintf("%d", l.cfg.CPUs),
	); err != nil {
		return fmt.Errorf("setting CPU count: %w", err)
	}

	// Disable secure boot for Linux
	if err := l.powershell(
		"Set-VMFirmware",
		"-VMName", l.vmName,
		"-EnableSecureBoot", "Off",
	); err != nil {
		l.cfg.Log.Warn("failed to disable secure boot", "error", err)
	}

	// Start the VM
	if err := l.powershell("Start-VM", "-Name", l.vmName); err != nil {
		return fmt.Errorf("starting VM: %w", err)
	}

	return nil
}

func (l *windowsLinuxVM) waitForContainerd() error {
	// Get the VM's IP address (assigned by Default Switch DHCP)
	var vmIP string
	l.cfg.Log.Info("waiting for Linux VM IP address")

	for i := range 60 {
		output, err := l.powershellOutput(
			"(Get-VMNetworkAdapter", "-VMName", l.vmName+").IPAddresses[0]",
		)
		if err == nil && output != "" {
			vmIP = output
			break
		}
		if i%10 == 0 && i > 0 {
			l.cfg.Log.Debug("still waiting for VM IP", "attempt", i)
		}
		time.Sleep(1 * time.Second)
	}

	if vmIP == "" {
		return fmt.Errorf("timed out waiting for VM IP address")
	}

	addr := fmt.Sprintf("%s:%d", vmIP, l.cfg.ContainerdPort)
	l.cfg.Log.Info("waiting for containerd in Linux VM", "address", addr)

	for i := range 60 {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			if i%10 == 0 && i > 0 {
				l.cfg.Log.Debug("still waiting for containerd", "attempt", i)
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
			l.cfg.Log.Info("containerd ready in Linux VM", "ip", vmIP)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("timed out waiting for containerd at %s", addr)
}

func (l *windowsLinuxVM) powershell(args ...string) error {
	cmd := exec.Command("powershell", append([]string{"-NoProfile", "-Command"}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (l *windowsLinuxVM) powershellOutput(args ...string) (string, error) {
	cmd := exec.Command("powershell", append([]string{"-NoProfile", "-Command"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
