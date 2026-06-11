//go:build darwin

package vm

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/crypto/ssh"
)

// darwinMacOSVM is an ephemeral macOS VM for a single job on macOS hosts.
// Uses Virtualization.framework with clone-on-write disk copies.
type darwinMacOSVM struct {
	cfg       MacOSVMConfig
	id        string
	vm        *vz.VirtualMachine
	clonePath string
	auxPath   string // per-job copy of aux storage (Vz locks it exclusively)
	jobDir    string // shared directory for JIT config exchange
	macAddr   string // VM's MAC address for ARP-based IP discovery
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewMacOSVM creates a new per-job macOS VM. Call Start() to boot it.
func NewMacOSVM(cfg MacOSVMConfig, jobID string) (MacOSVM, error) {
	cfg.SetDefaults()

	if cfg.DiskImage == "" {
		return nil, fmt.Errorf("MacOSVMConfig.DiskImage is required for macOS-native jobs")
	}
	if _, err := os.Stat(cfg.DiskImage); os.IsNotExist(err) {
		return nil, fmt.Errorf("macOS VM disk image not found: %s (expected EnsureMacOSBaseImage to have produced it)", cfg.DiskImage)
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
// directory so the macOS guest can pick it up on boot. Also links the
// runner tarball into the share so the guest can extract it.
func (m *darwinMacOSVM) WriteJITConfig(encodedJIT string) error {
	if err := os.MkdirAll(m.jobDir, 0o755); err != nil {
		return fmt.Errorf("creating job directory: %w", err)
	}
	jitPath := filepath.Join(m.jobDir, ".jit_config")
	if err := os.WriteFile(jitPath, []byte(encodedJIT), 0o644); err != nil {
		return fmt.Errorf("writing JIT config: %w", err)
	}

	// Write the ephemeral SSH public key so the guest can install it
	// into authorized_keys on boot. Rotated every daemon restart.
	if m.cfg.SSHPubKey != "" {
		if err := os.WriteFile(filepath.Join(m.jobDir, ".ssh_pubkey"), []byte(m.cfg.SSHPubKey), 0o644); err != nil {
			return fmt.Errorf("writing SSH public key to share: %w", err)
		}
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
	m.auxPath = filepath.Join(cloneDir, m.id+".aux")

	// Use APFS clone (cp -c) for instant copy-on-write
	if err := apfsCopy(ctx, m.cfg.DiskImage, m.clonePath, m.cfg.Log); err != nil {
		return fmt.Errorf("copying VM disk image: %w", err)
	}
	// Aux storage is locked exclusively by Vz — each VM needs its own copy.
	files := macOSVMFiles(m.cfg.DataDir)
	if err := apfsCopy(ctx, files.AuxStorage, m.auxPath, m.cfg.Log); err != nil {
		return fmt.Errorf("copying aux storage: %w", err)
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

	// macOS platform config — hardware model from the install, per-job aux
	// storage clone (Vz locks it exclusively), and a fresh machine identifier
	// so concurrent VMs don't collide.
	hwData, err := os.ReadFile(files.HardwareModel)
	if err != nil {
		cancel()
		return fmt.Errorf("reading hardware model: %w", err)
	}
	hw, err := vz.NewMacHardwareModelWithData(hwData)
	if err != nil {
		cancel()
		return fmt.Errorf("loading hardware model: %w", err)
	}
	mid, err := vz.NewMacMachineIdentifier()
	if err != nil {
		cancel()
		return fmt.Errorf("creating machine identifier: %w", err)
	}
	aux, err := vz.NewMacAuxiliaryStorage(m.auxPath)
	if err != nil {
		cancel()
		return fmt.Errorf("loading auxiliary storage: %w", err)
	}
	platformConfig, err := vz.NewMacPlatformConfiguration(
		vz.WithMacHardwareModel(hw),
		vz.WithMacMachineIdentifier(mid),
		vz.WithMacAuxiliaryStorage(aux),
	)
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

	// Shared directories:
	// 1. "ephemerd" → job dir (.jit_config, .ssh_pubkey, .ready sentinel)
	// 2. "runner"   → extracted GHA runner (run.sh, etc.)
	// Two shares avoid symlinks escaping the virtio-fs sandbox.
	var fsConfigs []vz.DirectorySharingDeviceConfiguration

	jobShare, err := vz.NewSharedDirectory(m.jobDir, false)
	if err != nil {
		cancel()
		return fmt.Errorf("creating job share: %w", err)
	}
	jobSingle, err := vz.NewSingleDirectoryShare(jobShare)
	if err != nil {
		cancel()
		return fmt.Errorf("creating job directory share: %w", err)
	}
	jobFS, err := vz.NewVirtioFileSystemDeviceConfiguration("ephemerd")
	if err != nil {
		cancel()
		return fmt.Errorf("creating job filesystem device: %w", err)
	}
	jobFS.SetDirectoryShare(jobSingle)
	fsConfigs = append(fsConfigs, jobFS)

	vmConfig.SetDirectorySharingDevicesVirtualMachineConfiguration(fsConfigs)

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

	// Remove stale .ready from a previous run of this job ID
	readyPath := filepath.Join(m.jobDir, ".ready")
	if err := os.Remove(readyPath); err != nil && !os.IsNotExist(err) {
		m.cfg.Log.Warn("failed to remove stale .ready file", "path", readyPath, "error", err)
	}

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

		// SSH fallback: once port 22 is open, SSH in using the ephemeral
		// key and start the runner directly from the host. This is more
		// reliable than LaunchDaemons (which may be blocked by SIP/SSV).
		conn, err := net.DialTimeout("tcp", ip+":22", 2*time.Second)
		if err != nil {
			if i%15 == 0 && i > 0 {
				m.cfg.Log.Debug("VM has IP but SSH not ready yet", "id", m.id, "ip", ip, "attempt", i)
			}
			continue
		}
		if err := conn.Close(); err != nil {
			m.cfg.Log.Debug("closing SSH probe connection", "error", err)
		}

		m.cfg.Log.Info("SSH reachable, setting up runner via SSH", "id", m.id, "ip", ip)
		if err := m.setupRunnerViaSSH(ctx, ip); err != nil {
			m.cfg.Log.Warn("SSH runner setup failed, will retry", "id", m.id, "error", err)
			continue
		}

		m.cfg.Log.Info("macOS VM runner reachable", "id", m.id, "ip", ip)
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
			_ = cmd.Run() // ignore errors — host may not respond
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
	// The macOS VM stays alive after the runner process exits (unlike
	// containers which exit with PID 1). Poll via SSH to detect when
	// the runner is done.
	ip := m.RunnerAddress()
	if ip != "" {
		go m.monitorRunner(ctx, ip)
	}

	select {
	case <-m.done:
		return 0, nil
	case <-ctx.Done():
		return 1, ctx.Err()
	}
}

// monitorRunner polls the VM via SSH to detect when the GitHub Actions
// runner process exits. When it does, stops the VM so Wait() returns.
func (m *darwinMacOSVM) monitorRunner(ctx context.Context, ip string) {
	// Build SSH config — try key first, fall back to password
	var authMethods []ssh.AuthMethod
	if m.cfg.SSHSigner != nil {
		if key, ok := m.cfg.SSHSigner.(ed25519.PrivateKey); ok {
			if signer, err := ssh.NewSignerFromKey(key); err == nil {
				authMethods = append(authMethods, ssh.PublicKeys(signer))
			}
		}
	}
	authMethods = append(authMethods, ssh.Password("admin"))

	sshCfg := &ssh.ClientConfig{
		User:            "admin",
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.done:
			return
		case <-ticker.C:
		}

		client, err := ssh.Dial("tcp", ip+":22", sshCfg)
		if err != nil {
			// SSH failed — VM may have crashed or been stopped
			m.cfg.Log.Info("runner monitor: SSH unreachable, stopping VM", "id", m.id)
			m.Stop()
			return
		}

		session, err := client.NewSession()
		if err != nil {
			if closeErr := client.Close(); closeErr != nil {
				m.cfg.Log.Debug("closing monitor SSH client", "error", closeErr)
			}
			continue
		}

		out, err := session.CombinedOutput("pgrep -f Runner.Listener || echo EXITED")
		if closeErr := session.Close(); closeErr != nil && closeErr.Error() != "EOF" {
			m.cfg.Log.Debug("closing monitor session", "error", closeErr)
		}
		if closeErr := client.Close(); closeErr != nil && closeErr.Error() != "EOF" {
			m.cfg.Log.Debug("closing monitor client", "error", closeErr)
		}

		if err != nil {
			m.cfg.Log.Debug("monitor pgrep error", "id", m.id, "error", err, "output", strings.TrimSpace(string(out)))
			continue
		}

		m.cfg.Log.Debug("monitor pgrep result", "id", m.id, "output", strings.TrimSpace(string(out)))

		if strings.TrimSpace(string(out)) == "EXITED" {
			// Give the runner a grace period to report results to GitHub
			// before we tear down the VM and network.
			m.cfg.Log.Info("runner process exited, waiting 30s for result upload", "id", m.id)
			select {
			case <-time.After(30 * time.Second):
			case <-ctx.Done():
			case <-m.done:
			}
			m.cfg.Log.Info("grace period complete, stopping VM", "id", m.id)
			m.Stop()
			return
		}
	}
}

// injectRunnerIntoClone mounts the APFS clone on the host and writes the
// runner files + JIT config directly to the filesystem. This eliminates the
// 60s SSH tar copy — the runner is already in place when the VM boots.
func (m *darwinMacOSVM) injectRunnerIntoClone(ctx context.Context) error {
	m.cfg.Log.Info("injecting runner into VM clone before boot", "id", m.id)

	dataVolume, detach, err := mountBaseImage(m.clonePath, m.cfg.Log)
	if err != nil {
		return fmt.Errorf("mounting clone: %w", err)
	}
	defer detach()

	// Copy the runner into /Users/admin/actions-runner/
	runnerDir := ""
	matches, _ := filepath.Glob(filepath.Join(m.cfg.DataDir, "runners", "*"))
	for _, d := range matches {
		if _, err := os.Stat(filepath.Join(d, "run.sh")); err == nil {
			runnerDir = d
			break
		}
	}
	if runnerDir == "" {
		return fmt.Errorf("no extracted runner found")
	}

	destRunner := filepath.Join(dataVolume, "Users", "admin", "actions-runner")
	if err := os.MkdirAll(destRunner, 0o755); err != nil {
		return fmt.Errorf("creating runner dir: %w", err)
	}

	// Use cp -R to copy the runner (faster than tar for local disk)
	if err := exec.CommandContext(ctx, "cp", "-R", runnerDir+"/.", destRunner+"/").Run(); err != nil {
		return fmt.Errorf("copying runner: %w", err)
	}

	// Write JIT config
	jitData, err := os.ReadFile(filepath.Join(m.jobDir, ".jit_config"))
	if err != nil {
		return fmt.Errorf("reading JIT config: %w", err)
	}
	jitDir := filepath.Join(dataVolume, "tmp", "ephemerd")
	if err := os.MkdirAll(jitDir, 0o755); err != nil {
		return fmt.Errorf("creating JIT dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(jitDir, ".jit_config"), jitData, 0o644); err != nil {
		return fmt.Errorf("writing JIT config: %w", err)
	}

	// Install ephemeral SSH public key for post-boot access (jobs ssh command)
	if m.cfg.SSHPubKey != "" {
		sshDir := filepath.Join(dataVolume, "Users", "admin", ".ssh")
		if err := os.MkdirAll(sshDir, 0o700); err != nil {
			return fmt.Errorf("creating .ssh dir: %w", err)
		}
		if err := os.WriteFile(filepath.Join(sshDir, "authorized_keys"), []byte(m.cfg.SSHPubKey), 0o600); err != nil {
			return fmt.Errorf("writing authorized_keys: %w", err)
		}
	}

	m.cfg.Log.Info("runner injected into clone", "id", m.id)
	return nil
}

// setupRunnerViaSSH connects to the VM using the Tart default credentials
// (admin/admin) and starts the GitHub Actions runner. This is more reliable
// than LaunchDaemons which may be blocked by SIP/SSV on modern macOS.
func (m *darwinMacOSVM) setupRunnerViaSSH(ctx context.Context, ip string) error {
	// Try ephemeral key first, fall back to password auth
	var authMethods []ssh.AuthMethod
	if m.cfg.SSHSigner != nil {
		if key, ok := m.cfg.SSHSigner.(ed25519.PrivateKey); ok {
			signer, err := ssh.NewSignerFromKey(key)
			if err == nil {
				authMethods = append(authMethods, ssh.PublicKeys(signer))
			}
		}
	}
	authMethods = append(authMethods, ssh.Password("admin"))

	sshCfg := &ssh.ClientConfig{
		User:            "admin",
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	client, err := ssh.Dial("tcp", ip+":22", sshCfg)
	if err != nil {
		return fmt.Errorf("SSH dial: %w", err)
	}
	defer func() { _ = client.Close() }()

	m.cfg.Log.Info("SSH connected to macOS VM", "id", m.id, "ip", ip)

	// Fix ownership FIRST as a blocking command — SSH strict mode rejects
	// key auth if /Users/admin or .ssh are owned by root (host injection).
	fixSession, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH session for chown: %w", err)
	}
	// Fix ownership AND re-install the SSH key in case the host-side injection
	// landed in the wrong place. This runs as a blocking command with password
	// auth (before password is randomized).
	fixCmd := fmt.Sprintf(`sudo chown admin:staff /Users/admin && sudo chown -R admin:staff /Users/admin/.ssh /Users/admin/actions-runner 2>/dev/null; mkdir -p ~/.ssh && echo '%s' > ~/.ssh/authorized_keys && chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys; echo ok`, strings.TrimSpace(m.cfg.SSHPubKey))
	if out, err := fixSession.CombinedOutput(fixCmd); err != nil {
		m.cfg.Log.Warn("chown failed", "output", string(out), "error", err)
	}
	if err := fixSession.Close(); err != nil {
		m.cfg.Log.Debug("closing fix session", "error", err)
	}

	// Read JIT config to pass inline via SSH
	jitData, err := os.ReadFile(filepath.Join(m.jobDir, ".jit_config"))
	if err != nil {
		return fmt.Errorf("reading JIT config: %w", err)
	}

	// Check if the runner exists in the VM. If not (host-side injection
	// failed due to APFS isolation), copy it via SSH tar pipe.
	checkSession, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH session for check: %w", err)
	}
	out, checkErr := checkSession.CombinedOutput("test -f /Library/ephemerd/runner/run.sh && echo found || echo missing")
	if closeErr := checkSession.Close(); closeErr != nil {
		m.cfg.Log.Debug("closing check session", "error", closeErr)
	}

	if checkErr != nil || strings.TrimSpace(string(out)) != "found" {
		// Runner not in clone — copy via SSH (uncompressed tar for speed)
		m.cfg.Log.Info("runner not in clone, copying via SSH", "id", m.id)
		runnerDir := ""
		matches, _ := filepath.Glob(filepath.Join(m.cfg.DataDir, "runners", "*"))
		for _, d := range matches {
			if _, sErr := os.Stat(filepath.Join(d, "run.sh")); sErr == nil {
				runnerDir = d
				break
			}
		}
		if runnerDir == "" {
			return fmt.Errorf("no extracted runner found in %s/runners/", m.cfg.DataDir)
		}

		tarSession, err := client.NewSession()
		if err != nil {
			return fmt.Errorf("SSH session for tar: %w", err)
		}
		stdin, err := tarSession.StdinPipe()
		if err != nil {
			if closeErr := tarSession.Close(); closeErr != nil {
				m.cfg.Log.Debug("closing tar session", "error", closeErr)
			}
			return fmt.Errorf("stdin pipe: %w", err)
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- tarSession.Run("sudo mkdir -p /Library/ephemerd/runner && sudo tar xf - -C /Library/ephemerd/runner")
		}()

		// Uncompressed tar (no -z) is faster — CPU isn't the bottleneck on localhost
		tarCmd := exec.CommandContext(ctx, "tar", "cf", "-", "-C", runnerDir, ".")
		tarCmd.Stdout = stdin
		if err := tarCmd.Run(); err != nil {
			if closeErr := stdin.Close(); closeErr != nil {
				m.cfg.Log.Debug("closing stdin after tar error", "error", closeErr)
			}
			return fmt.Errorf("tar runner: %w", err)
		}
		if err := stdin.Close(); err != nil {
			m.cfg.Log.Debug("closing tar stdin", "error", err)
		}

		if err := <-errCh; err != nil {
			return fmt.Errorf("untar on VM: %w", err)
		}
		m.cfg.Log.Info("runner copied to VM via SSH", "id", m.id)
	}

	// Start the runner + firewall in the background (fire and forget).
	// Runner binary is pre-installed in the base image (inherited by clone).
	// Only JIT config is per-job, passed inline.
	setupScript := fmt.Sprintf(`
# Firewall: block private networks EXCEPT the Vz NAT subnet
cat > /tmp/pf-ephemerd.conf << 'PFEOF'
pass quick to 192.168.64.0/24
block out quick to 10.0.0.0/8
block out quick to 172.16.0.0/12
block out quick to 192.168.0.0/16
block out quick to 169.254.0.0/16
pass out all
PFEOF
sudo pfctl -f /tmp/pf-ephemerd.conf -e 2>/dev/null || true

# Runner is pre-installed at /Library/ephemerd/runner/ (Data volume).
# Copy to home dir so the runner has a writable work directory.
RUNNER_SRC="/Library/ephemerd/runner"
RUNNER_DIR="/Users/admin/actions-runner"
if [ ! -f "$RUNNER_DIR/run.sh" ] && [ -f "$RUNNER_SRC/run.sh" ]; then
  cp -R "$RUNNER_SRC" "$RUNNER_DIR"
  chown -R admin:staff "$RUNNER_DIR"
fi
cd "$RUNNER_DIR"
./run.sh --jitconfig '%s' </dev/null >/tmp/runner.log 2>&1 &
RUNNER_PID=$!
disown $RUNNER_PID 2>/dev/null || true

# Randomize password LAST
RAND_PASS=$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 32)
dscl . -passwd /Users/admin admin "$RAND_PASS" 2>/dev/null || true

echo "runner started (pid=$RUNNER_PID)"
`, strings.TrimSpace(string(jitData)))

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH session for setup: %w", err)
	}

	if err := session.Start(setupScript); err != nil {
		if closeErr := session.Close(); closeErr != nil {
			m.cfg.Log.Debug("closing failed setup session", "error", closeErr)
		}
		return fmt.Errorf("starting setup script: %w", err)
	}

	time.Sleep(3 * time.Second)
	if err := session.Close(); err != nil {
		m.cfg.Log.Debug("closing setup session", "error", err)
	}

	// Write .ready on the host side
	readyPath := filepath.Join(m.jobDir, ".ready")
	if err := os.WriteFile(readyPath, []byte("1"), 0o644); err != nil {
		return fmt.Errorf("writing .ready: %w", err)
	}

	return nil
}

func (m *darwinMacOSVM) Stop() {
	m.cfg.Log.Info("stopping macOS VM", "id", m.id)

	if m.vm != nil {
		if m.vm.CanRequestStop() {
			if _, err := m.vm.RequestStop(); err != nil {
				m.cfg.Log.Debug("RequestStop failed", "id", m.id, "error", err)
			}
		}

		select {
		case <-m.done:
		case <-time.After(15 * time.Second):
			m.cfg.Log.Warn("macOS VM did not stop gracefully, forcing", "id", m.id)
			if err := m.vm.Stop(); err != nil {
				m.cfg.Log.Warn("force stop failed", "id", m.id, "error", err)
			}
		}
	}

	if m.cancel != nil {
		m.cancel()
	}

	// Delete the clones
	for _, path := range []string{m.clonePath, m.auxPath} {
		if path != "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				m.cfg.Log.Warn("failed to remove VM clone", "path", path, "error", err)
			}
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

// apfsCopy uses APFS clone-on-write (cp -c) for instant copies, falling
// back to a regular copy on non-APFS volumes.
func apfsCopy(ctx context.Context, src, dst string, log *slog.Logger) error {
	if err := exec.CommandContext(ctx, "cp", "-c", src, dst).Run(); err != nil {
		log.Warn("APFS clone failed, falling back to regular copy", "error", err)
		if err := exec.CommandContext(ctx, "cp", src, dst).Run(); err != nil {
			return err
		}
	}
	return nil
}
