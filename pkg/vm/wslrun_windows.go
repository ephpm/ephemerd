//go:build windows

package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// NewRunDistro creates a fresh WSL distro with a unique name for a single
// "ephemerd run" invocation. Call Destroy() when done.
func NewRunDistro(ctx context.Context, cfg RunDistroConfig) (*RunDistro, error) {
	d := &RunDistro{
		Name:    generateDistroName("ephemerd-run"),
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
	rootfsPath := filepath.Join(dir, "rootfs.tar.gz")
	if err := os.WriteFile(rootfsPath, rootfsData, 0o644); err != nil {
		return nil, fmt.Errorf("writing rootfs: %w", err)
	}

	ephemerdData, err := vmFS.ReadFile("embed/ephemerd-linux")
	if err != nil {
		return nil, fmt.Errorf("reading embedded ephemerd-linux: %w", err)
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
		if exitErr, ok := err.(*exec.ExitError); ok {
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
