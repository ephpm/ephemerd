//go:build windows

package vm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/hcsshim/hcn"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/defaults"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// hypervLinuxVM runs ephemerd inside a Hyper-V Linux VM for Linux container jobs.
// Unlike the previous WSL2 approach, this works from any Windows security context
// including LocalSystem (services), which WSL2 does not support.
//
// The VM is created using the HCS (Host Compute Service) API via vmcompute.dll.
// The HCS document structure matches what hcsshim's internal/uvm.makeLCOWDoc
// produces for a KernelDirect boot, but we call vmcompute.dll directly because
// Go's module system prevents importing hcsshim's internal packages.
//
// We intentionally skip hcsshim's uvm.CreateLCOW because it assumes a Microsoft
// GCS (Guest Compute Service) is running inside the VM -- it sets up vsock
// listeners for entropy injection and log forwarding that block Start() until
// the guest connects. Our VM runs a custom init that boots ephemerd-linux directly.
type hypervLinuxVM struct {
	cfg          LinuxVMConfig
	vmName       string // human-readable name for logging
	vmID         string // GUID used as the HCS compute system ID
	vmDir        string
	vmIP         string
	handle       hcsHandle
	endpointID   string // HCN endpoint on Default Switch
	client       *client.Client
	dispatchAddr string
	ipCh         chan string // console reader sends VM IP here
	cancel       context.CancelFunc
	done         chan struct{}
}

// StartLinuxVM creates a Hyper-V Linux VM from embedded kernel, initrd, and
// rootfs assets, boots it via the HCS API with LinuxKernelDirect, and waits
// for containerd + the dispatch gRPC server inside it to become ready.
func StartLinuxVM(cfg LinuxVMConfig) (LinuxVM, error) {
	cfg.SetDefaults()

	vmName, err := generateDistroName("ephemerd-linux")
	if err != nil {
		return nil, fmt.Errorf("generating VM name: %w", err)
	}

	// HCS compute system ID must be a GUID for GrantVmAccess to work.
	// GrantVmAccess uses the ID as a Windows security identifier (SID)
	// which requires GUID format — a human-readable string is rejected.
	vmID, err := newGUID()
	if err != nil {
		return nil, fmt.Errorf("generating VM GUID: %w", err)
	}

	l := &hypervLinuxVM{
		cfg:    cfg,
		vmName: vmName,
		vmID:   vmID,
		vmDir:  filepath.Join(cfg.DataDir, "vm", "linux"),
		ipCh:   make(chan string, 1),
		done:   make(chan struct{}),
	}

	l.cleanupStaleVMs()

	if err := l.extractAssets(); err != nil {
		return nil, fmt.Errorf("extracting VM assets: %w", err)
	}

	if err := l.ensureRootVHDX(); err != nil {
		return nil, fmt.Errorf("creating root VHDX: %w", err)
	}

	if err := l.createAndBootVM(); err != nil {
		l.Stop()
		return nil, fmt.Errorf("booting Hyper-V VM: %w", err)
	}

	if err := l.discoverIP(); err != nil {
		l.Stop()
		return nil, fmt.Errorf("discovering VM IP: %w", err)
	}

	if err := l.waitForContainerd(); err != nil {
		l.Stop()
		return nil, fmt.Errorf("containerd not ready in VM: %w", err)
	}

	return l, nil
}

func (l *hypervLinuxVM) Client() *client.Client {
	return l.client
}

func (l *hypervLinuxVM) DispatchAddr() string {
	return l.dispatchAddr
}

func (l *hypervLinuxVM) Stop() {
	l.cfg.Log.Info("stopping Linux VM (Hyper-V)", "name", l.vmName)

	if l.client != nil {
		if err := l.client.Close(); err != nil {
			l.cfg.Log.Warn("closing containerd client", "error", err)
		}
	}

	if l.cancel != nil {
		l.cancel()
	}

	// Wait for the VM monitor goroutine to finish
	select {
	case <-l.done:
	case <-time.After(10 * time.Second):
		l.cfg.Log.Warn("timed out waiting for VM monitor goroutine")
	}

	// Graceful shutdown, then force terminate
	if l.handle != 0 {
		if err := hcsShutDown(l.handle); err != nil {
			l.cfg.Log.Debug("HCS shutdown (may already be stopped)", "error", err)
		}
		// Give it a moment to shut down gracefully
		time.Sleep(1 * time.Second)
		if err := hcsTerminate(l.handle); err != nil {
			l.cfg.Log.Debug("HCS terminate (may already be stopped)", "error", err)
		}
		if err := hcsClose(l.handle); err != nil {
			l.cfg.Log.Warn("HCS close", "error", err)
		}
		l.handle = 0
	}

	// Remove the HCN endpoint (the Default Switch network itself persists)
	if l.endpointID != "" {
		ep, err := hcn.GetEndpointByID(l.endpointID)
		if err != nil {
			l.cfg.Log.Debug("HCN endpoint already removed", "id", l.endpointID, "error", err)
		} else {
			if err := ep.Delete(); err != nil {
				l.cfg.Log.Warn("deleting HCN endpoint", "id", l.endpointID, "error", err)
			}
		}
	}

	// Do NOT delete root VHDX at <data-dir>/containerd/linux-root/root.vhdx —
	// it persists across restarts so containerd images survive reboots.

	l.cfg.Log.Info("Linux VM stopped (Hyper-V)", "name", l.vmName)
}

// cleanupStaleVMs terminates any leftover ephemerd-linux-* VMs from a
// previous run or crash using the HCS enumerate API.
func (l *hypervLinuxVM) cleanupStaleVMs() {
	systems, err := hcsEnumerate("")
	if err != nil {
		l.cfg.Log.Warn("enumerating HCS compute systems for cleanup", "error", err)
		return
	}
	for _, sys := range systems {
		if sys.Owner != "ephemerd" {
			continue
		}
		l.cfg.Log.Info("cleaning up stale Hyper-V VM", "id", sys.ID, "owner", sys.Owner, "state", sys.State)
		l.terminateStaleVM(sys.ID)
	}
}

// terminateStaleVM force-stops a stale VM via the HCS Open API, falling
// back to PowerShell if Open is not available.
func (l *hypervLinuxVM) terminateStaleVM(id string) {
	handle, err := hcsOpen(id)
	if err != nil {
		// HcsOpen may fail if the VM is in a bad state -- fall back to PowerShell.
		// The id is a GUID (the HCS compute system ID), not a Hyper-V VM name,
		// so use Get-VM piped through Where-Object to match by Id property.
		l.cfg.Log.Debug("HCS open failed, trying PowerShell", "id", id, "error", err)
		script := fmt.Sprintf(`Get-VM | Where-Object { $_.Id -eq '%s' } | Stop-VM -Force -TurnOff -ErrorAction SilentlyContinue`, id)
		if err := psExec(script); err != nil {
			l.cfg.Log.Warn("PowerShell Stop-VM also failed", "id", id, "error", err)
		}
		return
	}
	if err := hcsTerminate(handle); err != nil {
		l.cfg.Log.Debug("terminating stale VM", "id", id, "error", err)
	}
	if err := hcsClose(handle); err != nil {
		l.cfg.Log.Warn("closing stale VM handle", "id", id, "error", err)
	}
}

// extractAssets writes the embedded kernel, initrd, rootfs, and ephemerd-linux
// binary to <DataDir>/vm/linux/.
func (l *hypervLinuxVM) extractAssets() error {
	if err := os.MkdirAll(l.vmDir, 0o755); err != nil {
		return fmt.Errorf("creating vm directory: %w", err)
	}

	type asset struct {
		src        string      // path in embed FS
		dest       string      // path on disk
		mode       os.FileMode
		gzip       bool        // expect gzip magic bytes
		findPrefix string      // if non-empty, use findEmbedded(prefix) for src
	}

	assets := []asset{
		{src: "embed/vmlinuz", dest: filepath.Join(l.vmDir, "vmlinuz"), mode: 0o644},
		{src: "embed/initrd", dest: filepath.Join(l.vmDir, "initrd"), mode: 0o644},
		// ephemerd-linux is NOT embedded separately — it's bundled inside the
		// fat initrd. The VM's init script extracts it at boot time.
		{findPrefix: "ephemerd-rootfs-", dest: filepath.Join(l.vmDir, "rootfs.tar.gz"), mode: 0o644, gzip: true},
	}

	for _, a := range assets {
		src := a.src
		if a.findPrefix != "" {
			found, err := findEmbedded(a.findPrefix)
			if err != nil {
				return fmt.Errorf("finding embedded %s: %w", a.findPrefix, err)
			}
			src = found
		}

		data, err := vmFS.ReadFile(src)
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", src, err)
		}
		if err := validateEmbeddedAsset(filepath.Base(src), data, a.gzip); err != nil {
			return err
		}

		// Compare SHA-256 to avoid re-writing unchanged assets
		if existingHash, err := fileSHA256Windows(a.dest); err == nil {
			if existingHash == sha256Bytes(data) {
				continue
			}
		}

		l.cfg.Log.Info("extracting embedded asset", "src", src, "dest", a.dest, "size", len(data))
		if err := os.WriteFile(a.dest, data, a.mode); err != nil {
			return fmt.Errorf("writing %s: %w", a.dest, err)
		}
	}

	return nil
}

// rootVHDXPath returns the path to the persistent VHDX that stores containerd's
// content store across VM restarts. Without this, every restart re-imports all
// images since the VM boots from tmpfs.
func (l *hypervLinuxVM) rootVHDXPath() string {
	return filepath.Join(l.cfg.DataDir, "containerd", "linux-root", "root.vhdx")
}

// ensureRootVHDX creates a sparse VHDX for the Linux VM's containerd root if
// it doesn't already exist. The VHDX persists across VM restarts so pulled and
// imported images survive reboots. After creating (or if the file already
// exists) we always re-run the VM-group ACL grant: the Hyper-V VM worker
// opens the VHDX under a virtual account that needs explicit access, and
// without it HcsStartComputeSystem returns HCS_E_SYSTEM_ALREADY_STOPPED.
func (l *hypervLinuxVM) ensureRootVHDX() error {
	vhdxPath := l.rootVHDXPath()

	if _, err := os.Stat(vhdxPath); err == nil {
		l.cfg.Log.Info("root VHDX already exists", "path", vhdxPath)
		return grantVmFileAccess(vhdxPath)
	}

	dir := filepath.Dir(vhdxPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}

	sizeGB := l.cfg.DiskSizeGB
	if sizeGB == 0 {
		sizeGB = 50
	}

	l.cfg.Log.Info("creating root VHDX for Linux VM containerd", "path", vhdxPath, "size_gb", sizeGB)

	// Use PowerShell's New-VHD to create a sparse (dynamic) VHDX.
	sizeBytes := uint64(sizeGB) * 1024 * 1024 * 1024
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		fmt.Sprintf("New-VHD -Path '%s' -SizeBytes %d -Dynamic | Out-Null", vhdxPath, sizeBytes))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("New-VHD failed: %w\noutput: %s", err, string(out))
	}

	return grantVmFileAccess(vhdxPath)
}

// grantVmFileAccess grants the Hyper-V VM worker's virtual account read+write
// access to the given file via icacls. Mirrors hcsshim's internal
// security.GrantVmGroupAccess which uses SetEntriesInAcl on the same SID but
// lives in an internal package we can't import. "Everyone" is used here for
// the same reason grant_windows.go in pkg/runtime uses it: virtual accounts
// on Windows 11 client do not reliably inherit membership in
// "NT VIRTUAL MACHINE\Virtual Machines" (S-1-5-83-0).
func grantVmFileAccess(path string) error {
	// Everyone:(M) = Modify rights. No inheritance flags — this is a file,
	// not a directory.
	cmd := exec.Command("icacls", path, "/grant", "*S-1-1-0:M", "/C", "/Q")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("icacls grant VM access on %s: %w: %s", path, err, string(out))
	}
	return nil
}

// sha256Bytes returns the hex-encoded SHA-256 hash of data.
func sha256Bytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// fileSHA256Windows computes the SHA-256 hash of a file on disk.
func fileSHA256Windows(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Warn("closing file for SHA-256", "path", path, "error", err)
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// LinuxKernelDirect boot, two SCSI disks (root + assets), and a network
// adapter on the Default Switch, then creates and starts the VM.
//
// The HCS document structure matches hcsshim's internal/uvm.makeLCOWDoc
// for a KernelDirect boot with schema version 2.1. Key elements:
// - LinuxKernelDirect chipset with kernel path, initrd path, and cmdline
// - SCSI controller using hcsshim's v5 UUID (ScsiControllerGuids[0])
// - Network adapter bound to an HCN endpoint on the Default Switch
// - COM port mapped to a named pipe for console output
func (l *hypervLinuxVM) createAndBootVM() error {
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel

	// Create an HCN endpoint on the Default Switch for VM networking
	endpointID, err := l.createNetworkEndpoint()
	if err != nil {
		cancel()
		return fmt.Errorf("creating network endpoint: %w", err)
	}
	l.endpointID = endpointID

	kernelPath := filepath.Join(l.vmDir, "vmlinuz")
	initrdPath := filepath.Join(l.vmDir, "initrd")
	consolePipe := fmt.Sprintf(`\\.\pipe\ephemerd-vm-%s`, l.vmName)

	// Kernel cmdline for initrd-based boot:
	// - rdinit=/init: run /init from the initrd (not from a root device)
	//   DO NOT use "root=/dev/sda init=/init" — that tells the kernel to mount
	//   /dev/sda as root and find init there, which fails on an unformatted VHDX.
	// - 8250_core: enable serial UART for console output via named pipe
	// - ephemerd.*: custom params parsed by our init script
	cmdline := fmt.Sprintf(
		"rdinit=/init ephemerd.containerd_port=%d ephemerd.root_disk=/dev/sda "+
			"pci=off brd.rd_nr=0 pmtmr=0 nr_cpus=%d "+
			"8250_core.nr_uarts=1 8250_core.skip_txen_test=1 console=ttyS0,115200",
		l.cfg.ContainerdPort, l.cfg.CPUs,
	)

	doc := &hcsComputeSystem{
		Owner: "ephemerd",
		SchemaVersion: &hcsVersion{
			Major: 2,
			Minor: 1,
		},
		ShouldTerminateOnLastHandleClosed: true,
		VirtualMachine: &hcsVM{
			StopOnReset: true,
			Chipset: &hcsChipset{
				LinuxKernelDirect: &hcsLinuxKernelDirect{
					KernelFilePath: kernelPath,
					InitRdPath:     initrdPath,
					KernelCmdLine:  cmdline,
				},
			},
			ComputeTopology: &hcsTopology{
				Memory: &hcsMemory{
					SizeInMB:        l.cfg.MemoryMB,
					AllowOvercommit: true,
				},
				Processor: &hcsProcessor{
					Count: uint32(l.cfg.CPUs),
				},
			},
			Devices: &hcsDevices{
				Scsi: map[string]hcsScsi{
					scsiControllerGUIDs[0]: {
						Attachments: map[string]hcsAttachment{
							// LUN 0: persistent VHDX for containerd root
							// (images, snapshots). Mounted at /var/lib/ephemerd/containerd/root
							// by the init script. Survives VM restarts.
							"0": {
								Type_: "VirtualDisk",
								Path:  l.rootVHDXPath(),
							},
						},
					},
				},
				NetworkAdapters: map[string]hcsNetworkAdapter{
					"default": {
						EndpointId: endpointID,
					},
				},
				ComPorts: map[string]hcsComPort{
					"0": {NamedPipe: consolePipe},
				},
				HvSocket: &hcsHvSocket{
					Config: &hcsHvSocketConfig{
						DefaultBindSecurityDescriptor: "D:P(A;;FA;;;SY)(A;;FA;;;BA)",
					},
				},
			},
		},
	}

	l.cfg.Log.Info("creating Hyper-V Linux VM",
		"name", l.vmName,
		"cpus", l.cfg.CPUs,
		"memory_mb", l.cfg.MemoryMB,
		"kernel", kernelPath,
		"initrd", initrdPath,
		"cmdline", cmdline,
	)

	handle, err := hcsCreate(l.vmID, doc)
	if err != nil {
		cancel()
		return fmt.Errorf("HCS create: %w", err)
	}
	l.handle = handle

	if err := hcsStart(handle); err != nil {
		if closeErr := hcsClose(handle); closeErr != nil {
			l.cfg.Log.Warn("closing HCS handle after start failure", "error", closeErr)
		}
		l.handle = 0
		cancel()
		return fmt.Errorf("HCS start: %w", err)
	}

	l.cfg.Log.Info("Hyper-V Linux VM started", "name", l.vmName)

	// No disk hot-add needed — everything runs from the fat initrd (tmpfs).

	// Read console output from the VM via named pipe and write to log file.
	// This captures kernel boot messages and init script output.
	go l.readConsole(consolePipe)

	// Monitor VM in background
	go l.monitorVM(ctx)

	return nil
}

// readConsole reads the VM console output from the named pipe and writes it
// to a log file. Also forwards fatal/panic lines to slog for visibility.
func (l *hypervLinuxVM) readConsole(consolePipe string) {
	logPath := filepath.Join(l.vmDir, "console.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		l.cfg.Log.Warn("could not create VM console log", "path", logPath, "error", err)
		return
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			l.cfg.Log.Warn("error closing console log", "error", err)
		}
	}()

	// Open the named pipe -- it may take a moment for HCS to create it
	var pipe net.Conn
	for i := range 10 {
		pipe, err = winio.DialPipe(consolePipe, nil)
		if err == nil {
			break
		}
		if i == 9 {
			l.cfg.Log.Warn("could not open VM console pipe", "pipe", consolePipe, "error", err)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	defer func() {
		if err := pipe.Close(); err != nil {
			l.cfg.Log.Warn("error closing console pipe", "error", err)
		}
	}()

	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if _, err := fmt.Fprintf(logFile, "%s\n", line); err != nil {
			l.cfg.Log.Warn("error writing to console log", "error", err)
		}
		// Parse IP from init script's EPHEMERD_IP= marker
		if strings.HasPrefix(line, "EPHEMERD_IP=") {
			ip := strings.TrimPrefix(line, "EPHEMERD_IP=")
			ip = strings.TrimSpace(ip)
			if ip != "" {
				select {
				case l.ipCh <- ip:
				default:
				}
			}
		}
		if strings.Contains(line, "FATAL") || strings.Contains(line, "panic") {
			l.cfg.Log.Error("[vm-console]", "line", line)
		}
	}
}

// monitorVM polls HCS enumerate to detect if the VM exits unexpectedly.
func (l *hypervLinuxVM) monitorVM(ctx context.Context) {
	defer close(l.done)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			systems, err := hcsEnumerate("")
			if err != nil {
				continue
			}
			found := false
			for _, sys := range systems {
				if sys.ID == l.vmID {
					found = true
					break
				}
			}
			if !found {
				l.cfg.Log.Info("Hyper-V Linux VM exited", "name", l.vmName)
				return
			}
		}
	}
}

// createNetworkEndpoint creates an HCN endpoint on the Default Switch
// for the VM's network adapter.
func (l *hypervLinuxVM) createNetworkEndpoint() (string, error) {
	network, err := hcn.GetNetworkByName("Default Switch")
	if err != nil {
		return "", fmt.Errorf("finding Default Switch: %w (is Hyper-V enabled?)", err)
	}

	endpoint := &hcn.HostComputeEndpoint{
		Name:               l.vmName + "-ep",
		HostComputeNetwork: network.Id,
		SchemaVersion: hcn.SchemaVersion{
			Major: 2,
			Minor: 0,
		},
	}

	created, err := network.CreateEndpoint(endpoint)
	if err != nil {
		return "", fmt.Errorf("creating HCN endpoint: %w", err)
	}

	l.cfg.Log.Info("HCN endpoint created on Default Switch",
		"name", created.Name,
		"id", created.Id,
	)
	return created.Id, nil
}

// discoverIP waits for the VM's init script to report its IP address via the
// console pipe (EPHEMERD_IP=x.x.x.x marker line parsed by readConsole).
func (l *hypervLinuxVM) discoverIP() error {
	l.cfg.Log.Info("waiting for VM IP address", "name", l.vmName)

	select {
	case ip := <-l.ipCh:
		l.vmIP = ip
		l.cfg.Log.Info("VM IP discovered", "ip", l.vmIP, "name", l.vmName)
		return nil
	case <-l.done:
		return fmt.Errorf("VM exited before IP was discovered")
	case <-time.After(60 * time.Second):
		return fmt.Errorf("timed out waiting for VM IP (60s)")
	}
}

// waitForContainerd polls the TCP port until containerd responds, then waits
// for the dispatch gRPC server on the next port.
func (l *hypervLinuxVM) waitForContainerd() error {
	addr := net.JoinHostPort(l.vmIP, fmt.Sprintf("%d", l.cfg.ContainerdPort))
	l.cfg.Log.Info("waiting for containerd in Hyper-V VM", "address", addr)

	var lastErr error
	for i := range 120 {
		select {
		case <-l.done:
			return fmt.Errorf("VM exited before containerd was ready")
		default:
		}

		tcpConn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			lastErr = err
			if i%15 == 0 && i > 0 {
				l.cfg.Log.Debug("still waiting for containerd in VM", "attempt", i)
			}
			time.Sleep(1 * time.Second)
			continue
		}
		if err := tcpConn.Close(); err != nil {
			l.cfg.Log.Debug("closing TCP probe connection", "error", err)
		}

		// containerd's Windows dialer only supports named pipes, so we
		// bypass it with a direct gRPC TCP connection.
		grpcConn, err := grpc.NewClient(addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize),
				grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize),
			),
		)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		l.client, err = client.NewWithConn(grpcConn)
		if err != nil {
			lastErr = err
			if closeErr := grpcConn.Close(); closeErr != nil {
				l.cfg.Log.Debug("closing gRPC connection after client error", "error", closeErr)
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = l.client.Version(ctx)
		cancel()
		if err == nil {
			l.cfg.Log.Info("containerd ready in Hyper-V VM", "address", addr)
			break
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}

	if l.client == nil {
		return fmt.Errorf("timed out waiting for containerd at %s: %w", addr, lastErr)
	}

	// Wait for the dispatch gRPC server on containerdPort + 1.
	// The scheduler routes Linux jobs through this so containers get full
	// CNI networking via ephemerd-linux instead of being created over the
	// raw containerd API.
	dispatchAddr := net.JoinHostPort(l.vmIP, fmt.Sprintf("%d", l.cfg.ContainerdPort+1))
	l.cfg.Log.Info("waiting for dispatch server in Hyper-V VM", "address", dispatchAddr)

	// The dispatch server starts after containerd + runner extraction + CNI +
	// networking are all ready inside the VM. On first run the runner extraction
	// can take 30-60s, so allow up to 90s total.
	for i := range 90 {
		select {
		case <-l.done:
			return fmt.Errorf("VM exited before dispatch server was ready")
		default:
		}

		conn, err := net.DialTimeout("tcp", dispatchAddr, 2*time.Second)
		if err != nil {
			if i%15 == 0 && i > 0 {
				l.cfg.Log.Debug("still waiting for dispatch server in VM", "attempt", i)
			}
			time.Sleep(1 * time.Second)
			continue
		}
		if err := conn.Close(); err != nil {
			l.cfg.Log.Debug("closing dispatch probe connection", "error", err)
		}

		l.dispatchAddr = dispatchAddr
		l.cfg.Log.Info("dispatch server ready in Hyper-V VM", "address", dispatchAddr)
		return nil
	}

	return fmt.Errorf("timed out waiting for dispatch server at %s", dispatchAddr)
}

// --- Shared helpers (used by wslrun_windows.go) ---

// findEmbedded finds a file in the embed FS by prefix, skipping placeholder
// files created by EnsurePlaceholders for compile-time compatibility.
func findEmbedded(prefix string) (string, error) {
	entries, err := vmFS.ReadDir("embed")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && !strings.Contains(e.Name(), "placeholder") {
			return "embed/" + e.Name(), nil
		}
	}
	return "", fmt.Errorf("no embedded file with prefix %q found (binary may have been built without 'mage windows' -- only the placeholder rootfs is embedded)", prefix)
}

// validateEmbeddedAsset checks that embedded data is not a build-time placeholder.
// Verifies the data is non-empty and, for gzip files, starts with the gzip magic
// bytes (0x1f 0x8b). Returns a descriptive error if validation fails.
func validateEmbeddedAsset(name string, data []byte, expectGzip bool) error {
	if len(data) == 0 {
		return fmt.Errorf("embedded %s is empty (0 bytes) -- binary was built without real VM assets; rebuild with 'mage windows'", name)
	}
	if expectGzip && len(data) >= 2 && (data[0] != 0x1f || data[1] != 0x8b) {
		return fmt.Errorf("embedded %s is not a valid gzip file (got magic %02x %02x) -- binary was built without real VM assets; rebuild with 'mage windows'", name, data[0], data[1])
	}
	return nil
}

// --- PowerShell helpers ---

// psExec runs a PowerShell script and returns an error if it fails.
// The script's stdout/stderr are logged on failure.
func psExec(script string) error {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return fmt.Errorf("powershell: %w: %s", err, detail)
		}
		return fmt.Errorf("powershell: %w", err)
	}
	return nil
}

