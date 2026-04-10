//go:build windows

package vm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/defaults"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const wslCmdTimeout = 30 * time.Second

const distroName = "ephemerd-linux"

// wslLinuxVM runs ephemerd inside a WSL2 distro for Linux container jobs.
type wslLinuxVM struct {
	cfg        LinuxVMConfig
	installDir string
	client     *client.Client
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	done       chan struct{}
}

// StartLinuxVM creates a WSL2 distro from an embedded Alpine rootfs,
// copies the embedded Linux ephemerd binary into it, and launches
// ephemerd which starts its own in-process containerd.
func StartLinuxVM(cfg LinuxVMConfig) (LinuxVM, error) {
	cfg.SetDefaults()

	l := &wslLinuxVM{
		cfg:        cfg,
		installDir: filepath.Join(cfg.DataDir, "vm", "linux", "distro"),
		done:       make(chan struct{}),
	}

	// Clean up any stale distro from a previous run/crash
	l.cleanupStaleDistro()

	if err := l.extractAssets(); err != nil {
		return nil, fmt.Errorf("extracting VM assets: %w", err)
	}

	if err := l.importDistro(); err != nil {
		return nil, fmt.Errorf("importing WSL distro: %w", err)
	}

	if err := l.installEphemerd(); err != nil {
		l.destroy()
		return nil, fmt.Errorf("installing ephemerd in WSL: %w", err)
	}

	if err := l.launch(); err != nil {
		l.destroy()
		return nil, fmt.Errorf("launching ephemerd in WSL: %w", err)
	}

	if err := l.waitForContainerd(); err != nil {
		l.Stop()
		return nil, fmt.Errorf("containerd not ready in WSL: %w", err)
	}

	return l, nil
}

func (l *wslLinuxVM) Client() *client.Client {
	return l.client
}

func (l *wslLinuxVM) Stop() {
	l.cfg.Log.Info("stopping Linux VM (WSL)")

	if l.client != nil {
		_ = l.client.Close()
	}

	if l.cancel != nil {
		l.cancel()
	}

	// Wait for the ephemerd process to exit
	select {
	case <-l.done:
	case <-time.After(10 * time.Second):
		l.cfg.Log.Warn("timed out waiting for WSL ephemerd to exit, force terminating")
	}

	l.destroy()
	l.cfg.Log.Info("Linux VM stopped (WSL)")
}

// destroy terminates and unregisters the WSL distro.
// Uses timeouts to avoid hanging if WSL service is stuck.
func (l *wslLinuxVM) destroy() {
	if err := wslExecTimeout(wslCmdTimeout, "--terminate", distroName); err != nil {
		l.cfg.Log.Warn("wsl --terminate failed or timed out", "error", err)
	}
	if err := wslExecTimeout(wslCmdTimeout, "--unregister", distroName); err != nil {
		l.cfg.Log.Warn("wsl --unregister failed or timed out", "error", err)
	}
}

// cleanupStaleDistro removes any leftover distro from a crash.
// If WSL commands hang (stuck wslservice), it kills wslservice.exe
// as a last resort — the OS auto-restarts it with a fresh state.
func (l *wslLinuxVM) cleanupStaleDistro() {
	out, err := wslOutputTimeout(wslCmdTimeout, "--list", "--quiet")
	if err != nil {
		l.cfg.Log.Warn("wsl --list timed out or failed, attempting wslservice restart", "error", err)
		killWSLService(l.cfg.Log)
		// Retry once after the restart
		out, err = wslOutputTimeout(wslCmdTimeout, "--list", "--quiet")
		if err != nil {
			l.cfg.Log.Warn("wsl --list still failing after wslservice restart", "error", err)
			return
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == distroName {
			l.cfg.Log.Info("cleaning up stale WSL distro", "name", distroName)
			if err := wslExecTimeout(wslCmdTimeout, "--terminate", distroName); err != nil {
				l.cfg.Log.Warn("wsl --terminate timed out, killing wslservice", "error", err)
				killWSLService(l.cfg.Log)
			}
			_ = wslExecTimeout(wslCmdTimeout, "--unregister", distroName)
			return
		}
	}
}

// extractAssets writes the embedded rootfs and Linux binary to disk.
func (l *wslLinuxVM) extractAssets() error {
	dir := filepath.Join(l.cfg.DataDir, "vm", "linux")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating vm directory: %w", err)
	}

	// Extract Alpine rootfs
	rootfsName, err := findEmbedded("alpine-minirootfs-")
	if err != nil {
		return fmt.Errorf("finding rootfs: %w", err)
	}
	rootfsData, err := vmFS.ReadFile(rootfsName)
	if err != nil {
		return fmt.Errorf("reading embedded rootfs: %w", err)
	}
	rootfsPath := filepath.Join(dir, "rootfs.tar.gz")
	if err := os.WriteFile(rootfsPath, rootfsData, 0o644); err != nil {
		return fmt.Errorf("writing rootfs: %w", err)
	}

	// Extract Linux ephemerd binary
	ephemerdData, err := vmFS.ReadFile("embed/ephemerd-linux")
	if err != nil {
		return fmt.Errorf("reading embedded ephemerd-linux: %w", err)
	}
	ephemerdPath := filepath.Join(dir, "ephemerd-linux")
	if err := os.WriteFile(ephemerdPath, ephemerdData, 0o755); err != nil {
		return fmt.Errorf("writing ephemerd-linux: %w", err)
	}

	return nil
}

// findEmbedded finds a file in the embed FS by prefix.
func findEmbedded(prefix string) (string, error) {
	entries, err := vmFS.ReadDir("embed")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			return "embed/" + e.Name(), nil
		}
	}
	return "", fmt.Errorf("no embedded file with prefix %q found", prefix)
}

// importDistro creates the WSL distro from the Alpine rootfs.
func (l *wslLinuxVM) importDistro() error {
	l.cfg.Log.Info("importing WSL distro", "name", distroName)

	rootfsPath := filepath.Join(l.cfg.DataDir, "vm", "linux", "rootfs.tar.gz")
	if err := os.MkdirAll(l.installDir, 0o755); err != nil {
		return fmt.Errorf("creating distro directory: %w", err)
	}

	return wslExec("--import", distroName, l.installDir, rootfsPath)
}

// installEphemerd copies the Linux binary into the WSL distro via UNC path.
func (l *wslLinuxVM) installEphemerd() error {
	l.cfg.Log.Info("installing ephemerd in WSL distro")

	// Create target directory inside the distro
	if err := wslExec("-d", distroName, "--", "mkdir", "-p", "/opt/ephemerd"); err != nil {
		return fmt.Errorf("creating /opt/ephemerd: %w", err)
	}

	// Copy binary via UNC path
	src := filepath.Join(l.cfg.DataDir, "vm", "linux", "ephemerd-linux")
	dst := fmt.Sprintf(`\\wsl$\%s\opt\ephemerd\ephemerd`, distroName)

	srcData, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("reading ephemerd-linux: %w", err)
	}
	if err := os.WriteFile(dst, srcData, 0o755); err != nil {
		return fmt.Errorf("writing to WSL UNC path: %w", err)
	}

	// Ensure executable
	if err := wslExec("-d", distroName, "--", "chmod", "+x", "/opt/ephemerd/ephemerd"); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	// Ensure /var/lib/ephemerd exists for config and PEM
	if err := wslExec("-d", distroName, "--", "mkdir", "-p", "/var/lib/ephemerd"); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	// Copy the GitHub App PEM file into the distro
	if l.cfg.PrivateKeyPath != "" {
		pemData, err := os.ReadFile(l.cfg.PrivateKeyPath)
		if err != nil {
			return fmt.Errorf("reading PEM file: %w", err)
		}
		pemDst := fmt.Sprintf(`\\wsl$\%s\var\lib\ephemerd\app.pem`, distroName)
		if err := os.WriteFile(pemDst, pemData, 0o600); err != nil {
			return fmt.Errorf("writing PEM to WSL: %w", err)
		}
		l.cfg.Log.Info("copied GitHub App PEM into WSL")
	}

	// Copy host config file into the distro so the inner ephemerd has GitHub/runner config
	if l.cfg.ConfigFile != "" {
		configData, err := os.ReadFile(l.cfg.ConfigFile)
		if err != nil {
			return fmt.Errorf("reading config file: %w", err)
		}

		// Rewrite the PEM path to the Linux-side location
		if l.cfg.PrivateKeyPath != "" {
			re := regexp.MustCompile(`(?m)^(\s*private_key_path\s*=\s*).*$`)
			configData = re.ReplaceAll(configData, []byte(`${1}"/var/lib/ephemerd/app.pem"`))
		}

		configDst := fmt.Sprintf(`\\wsl$\%s\var\lib\ephemerd\config.toml`, distroName)
		if err := os.WriteFile(configDst, configData, 0o644); err != nil {
			return fmt.Errorf("writing config to WSL: %w", err)
		}
	}

	// Install gcompat (glibc compat for containerd-shim-runc-v2) and iptables (for CNI/firewall).
	// Done last because WSL networking may not be ready immediately after import.
	// Retry with a timeout to handle transient DNS failures.
	l.cfg.Log.Info("installing dependencies in WSL distro (gcompat, iptables)")
	const apkTimeout = 2 * time.Minute
	for attempt := range 3 {
		if err := wslExecTimeout(apkTimeout, "-d", distroName, "--", "apk", "add", "--no-cache", "gcompat", "iptables"); err != nil {
			l.cfg.Log.Warn("apk install attempt failed", "attempt", attempt+1, "error", err)
			if attempt < 2 {
				time.Sleep(3 * time.Second)
				continue
			}
			return fmt.Errorf("installing dependencies: %w", err)
		}
		break
	}

	return nil
}

// launch starts ephemerd inside the WSL distro.
func (l *wslLinuxVM) launch() error {
	l.cfg.Log.Info("launching ephemerd in WSL", "port", l.cfg.ContainerdPort)

	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel

	l.cmd = exec.CommandContext(ctx, "wsl", "-d", distroName, "--",
		"/opt/ephemerd/ephemerd", "serve",
		"--data-dir", "/var/lib/ephemerd",
		"--containerd-tcp-port", fmt.Sprintf("%d", l.cfg.ContainerdPort),
	)

	// Pipe stdout/stderr to our logger
	stdout, err := l.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := l.cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := l.cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("starting WSL ephemerd: %w", err)
	}

	// Forward output directly to stderr with explicit \r\n line endings.
	// PowerShell's terminal needs \r to reset cursor to column 0; routing
	// through slog drops the \r and produces stair-step output.
	forward := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			fmt.Fprintf(os.Stderr, "[wsl-linux] %s\r\n", scanner.Text())
		}
	}
	go forward(stdout)
	go forward(stderr)

	// Wait for process exit in background
	go func() {
		defer close(l.done)
		if err := l.cmd.Wait(); err != nil {
			select {
			case <-ctx.Done():
			default:
				l.cfg.Log.Error("WSL ephemerd exited with error", "error", err)
			}
		}
	}()

	return nil
}

// waitForContainerd polls the TCP port until containerd responds.
func (l *wslLinuxVM) waitForContainerd() error {
	addr := fmt.Sprintf("localhost:%d", l.cfg.ContainerdPort)
	l.cfg.Log.Info("waiting for containerd in WSL", "address", addr)

	var lastErr error
	for i := range 60 {
		// Check if the process died
		select {
		case <-l.done:
			return fmt.Errorf("WSL ephemerd exited before containerd was ready")
		default:
		}

		tcpConn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			lastErr = err
			if i%10 == 0 && i > 0 {
				l.cfg.Log.Debug("still waiting for containerd in WSL", "attempt", i)
			}
			time.Sleep(1 * time.Second)
			continue
		}
		tcpConn.Close()

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
			time.Sleep(500 * time.Millisecond)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = l.client.Version(ctx)
		cancel()
		if err == nil {
			l.cfg.Log.Info("containerd ready in WSL", "address", addr)
			return nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("timed out waiting for containerd at %s: %w", addr, lastErr)
}

func wslExec(args ...string) error {
	cmd := exec.Command("wsl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func wslExecTimeout(timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wsl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("wsl %s: timed out after %s", strings.Join(args, " "), timeout)
		}
		return err
	}
	return nil
}

func wslOutput(args ...string) (string, error) {
	cmd := exec.Command("wsl", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(decodeWSLOutput(out)), nil
}

func wslOutputTimeout(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wsl", args...)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("wsl %s: timed out after %s", strings.Join(args, " "), timeout)
		}
		return "", err
	}
	return strings.TrimSpace(decodeWSLOutput(out)), nil
}

// decodeWSLOutput converts WSL's UTF-16LE output (from wsl --list) to UTF-8.
// If the output doesn't look like UTF-16LE, it's returned as-is.
func decodeWSLOutput(b []byte) string {
	// UTF-16LE BOM is FF FE; even without BOM, wsl --list outputs UTF-16LE
	// which shows up as alternating bytes with nulls.
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		b = b[2:] // strip BOM
	}
	// Must be even length for UTF-16
	if len(b)%2 != 0 {
		return string(b)
	}
	// Quick check: if no null bytes, it's probably already UTF-8
	hasNull := false
	for i := 1; i < len(b); i += 2 {
		if b[i] == 0 {
			hasNull = true
			break
		}
	}
	if !hasNull {
		return string(b)
	}
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u16))
}

// killWSLService force-kills wslservice.exe to unstick a hung WSL.
// The OS auto-restarts the service with a fresh state.
func killWSLService(log *slog.Logger) {
	log.Warn("killing wslservice.exe to recover from stuck WSL")
	cmd := exec.Command("taskkill", "/F", "/IM", "wslservice.exe")
	if err := cmd.Run(); err != nil {
		log.Warn("taskkill wslservice.exe failed (may not be running)", "error", err)
	}
	// Give the OS a moment to restart the service
	time.Sleep(2 * time.Second)
}
