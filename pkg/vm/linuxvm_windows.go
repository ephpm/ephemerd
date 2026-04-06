//go:build windows

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

	"github.com/containerd/containerd/v2/client"
)

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
func (l *wslLinuxVM) destroy() {
	_ = wslExec("--terminate", distroName)
	_ = wslExec("--unregister", distroName)
}

// cleanupStaleDistro removes any leftover distro from a crash.
func (l *wslLinuxVM) cleanupStaleDistro() {
	out, err := wslOutput("--list", "--quiet")
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == distroName {
			l.cfg.Log.Info("cleaning up stale WSL distro", "name", distroName)
			_ = wslExec("--terminate", distroName)
			_ = wslExec("--unregister", distroName)
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

	// Forward output in background
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			l.cfg.Log.Info("[wsl-linux] " + scanner.Text())
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			l.cfg.Log.Warn("[wsl-linux] " + scanner.Text())
		}
	}()

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

		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			lastErr = err
			if i%10 == 0 && i > 0 {
				l.cfg.Log.Debug("still waiting for containerd in WSL", "attempt", i)
			}
			time.Sleep(1 * time.Second)
			continue
		}
		conn.Close()

		l.client, err = client.New("tcp://"+addr)
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

func wslOutput(args ...string) (string, error) {
	cmd := exec.Command("wsl", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
