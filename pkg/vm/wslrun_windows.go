//go:build windows

package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

// NewRunDistro creates a fresh WSL distro with a unique name for a single
// "ephemerd run" invocation. Call Destroy() when done.
func NewRunDistro(ctx context.Context, cfg RunDistroConfig) (*RunDistro, error) {
	name, err := generateDistroName("ephemerd-run")
	if err != nil {
		return nil, fmt.Errorf("generating distro name: %w", err)
	}

	d := &RunDistro{
		Name:    name,
		dataDir: cfg.DataDir,
		log:     cfg.Log,
	}

	d.log.Info("setting up WSL run distro", "name", d.Name)

	// Per-distro directory so concurrent distros don't share filesystem state
	dir := filepath.Join(d.dataDir, "vm", "run", d.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating vm/run/%s directory: %w", d.Name, err)
	}

	rootfsName, err := findEmbedded("ephemerd-rootfs-")
	if err != nil {
		return nil, fmt.Errorf("finding rootfs: %w", err)
	}
	rootfsData, err := vmFS.ReadFile(rootfsName)
	if err != nil {
		return nil, fmt.Errorf("reading embedded rootfs: %w", err)
	}
	if err := validateEmbeddedAsset("rootfs", rootfsData, true); err != nil {
		return nil, err
	}
	rootfsPath := filepath.Join(dir, "rootfs.tar.gz")
	if err := os.WriteFile(rootfsPath, rootfsData, 0o644); err != nil {
		return nil, fmt.Errorf("writing rootfs: %w", err)
	}

	// Read ephemerd-linux from disk — it's extracted from the initrd by the
	// daemon's extractAssets on first run. Not embedded separately to avoid
	// double-embedding (it's already bundled inside the fat initrd).
	srcBinary := filepath.Join(d.dataDir, "vm", "linux", "ephemerd-linux")
	ephemerdData, err := os.ReadFile(srcBinary)
	if err != nil {
		return nil, fmt.Errorf("reading %s (has 'ephemerd serve' run once?): %w", srcBinary, err)
	}
	if err := validateEmbeddedAsset("ephemerd-linux", ephemerdData, false); err != nil {
		return nil, err
	}
	ephemerdPath := filepath.Join(dir, "ephemerd-linux")
	if err := os.WriteFile(ephemerdPath, ephemerdData, 0o755); err != nil {
		return nil, fmt.Errorf("writing ephemerd-linux: %w", err)
	}

	// Import distro
	installDir := filepath.Join(dir, "distro")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating distro directory: %w", err)
	}
	if err := wslExec("--import", d.Name, installDir, rootfsPath); err != nil {
		return nil, fmt.Errorf("wsl --import: %w", err)
	}

	// Create data directory inside distro (binary runs from /mnt/c/ directly)
	if err := wslExec("-d", d.Name, "--", "mkdir", "-p", "/var/lib/ephemerd"); err != nil {
		d.Destroy()
		return nil, fmt.Errorf("creating data dir: %w", err)
	}

	d.log.Info("WSL run distro ready", "name", d.Name)
	return d, nil
}

// wslBinaryPath returns the /mnt/ path to the extracted Linux binary,
// accessible from inside WSL without copying it into the distro.
// e.g. C:\ProgramData\ephemerd\vm\run\ephemerd-run-abc123\ephemerd-linux
//
//	→ /mnt/c/ProgramData/ephemerd/vm/run/ephemerd-run-abc123/ephemerd-linux
func (d *RunDistro) wslBinaryPath() string {
	winPath := filepath.Join(d.dataDir, "vm", "run", d.Name, "ephemerd-linux")
	drive := strings.ToLower(string(winPath[0]))
	rest := filepath.ToSlash(winPath[2:]) // skip "C:"
	return "/mnt/" + drive + rest
}

// Run delegates an ephemerd run invocation to this distro.
// The binary runs directly from /mnt/c/ (the Windows filesystem) — no copy needed.
// Stdout/stderr/stdin are connected directly for interactive output.
// Returns the exit code from the WSL process.
func (d *RunDistro) Run(ctx context.Context, cfg RunInWSLConfig) (int, error) {
	wslWorkflow, err := WindowsPathToWSL(cfg.WorkflowPath)
	if err != nil {
		return 1, fmt.Errorf("translating workflow path: %w", err)
	}

	wslRepo, err := WindowsPathToWSL(cfg.RepoDir)
	if err != nil {
		return 1, fmt.Errorf("translating repo path: %w", err)
	}

	binPath := d.wslBinaryPath()
	args := []string{
		"-d", d.Name,
		"--cd", wslRepo,
		"--",
		binPath, "run",
		"--data-dir", "/var/lib/ephemerd",
	}
	if cfg.JobFilter != "" {
		args = append(args, "--job", cfg.JobFilter)
	}
	args = append(args, wslWorkflow)

	d.log.Info("delegating to WSL",
		"distro", d.Name,
		"workflow", wslWorkflow,
		"repo", wslRepo,
	)

	cmd := exec.CommandContext(ctx, "wsl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("running ephemerd in WSL: %w", err)
	}
	return 0, nil
}

// Destroy terminates and unregisters this distro's WSL instance and
// removes its per-distro directory.
func (d *RunDistro) Destroy() {
	d.log.Info("destroying WSL run distro", "name", d.Name)
	if err := wslExecTimeout(wslCmdTimeout, "--terminate", d.Name); err != nil {
		d.log.Warn("wsl --terminate failed", "error", err)
	}
	if err := wslExecTimeout(wslCmdTimeout, "--unregister", d.Name); err != nil {
		d.log.Warn("wsl --unregister failed", "error", err)
	}
	// Clean up per-distro directory
	dir := filepath.Join(d.dataDir, "vm", "run", d.Name)
	if err := os.RemoveAll(dir); err != nil {
		d.log.Warn("removing distro directory failed", "path", dir, "error", err)
	}
}

const wslCmdTimeout = 30 * time.Second

func wslExec(args ...string) error {
	cmd := exec.Command("wsl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(decodeWSLOutput(out))
		if detail != "" {
			return fmt.Errorf("wsl %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return fmt.Errorf("wsl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func wslExecTimeout(timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wsl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("wsl %s: timed out after %s", strings.Join(args, " "), timeout)
		}
		detail := strings.TrimSpace(decodeWSLOutput(out))
		if detail != "" {
			return fmt.Errorf("wsl %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return fmt.Errorf("wsl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// decodeWSLOutput converts WSL's UTF-16LE output (from wsl --list) to UTF-8.
// If the output doesn't look like UTF-16LE, it's returned as-is.
func decodeWSLOutput(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		b = b[2:]
	}
	if len(b)%2 != 0 {
		return string(b)
	}
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

