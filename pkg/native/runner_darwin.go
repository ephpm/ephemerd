// Package native implements a macOS-native GitHub Actions runner environment.
// Jobs run directly on the host inside a sandbox-exec profile with per-job
// workspace isolation. Faster than VM mode but weaker isolation — intended
// for trusted workloads.
package native

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Config configures a single native macOS runner job.
type Config struct {
	DataDir   string       // ephemerd data directory
	JobID     string       // unique job identifier
	JITConfig string       // encoded JIT runner config from the provider
	RunnerDir string       // path to extracted GHA runner template (shared, read-only)
	Log       *slog.Logger // structured logger
}

// Runner manages a single native macOS job lifecycle: workspace creation,
// sandbox-exec launch, process group management, and cleanup.
type Runner struct {
	cfg       Config
	workspace string // per-job workspace root
	cmd       *exec.Cmd
	done      chan struct{} // closed when cmd.Wait returns
	exitCode  int
	waitErr   error
}

// New creates a new native macOS runner. It sets up the per-job workspace
// directories but does not start the runner process.
func New(cfg Config) (*Runner, error) {
	workspace := filepath.Join(cfg.DataDir, "native-jobs", cfg.JobID)
	dirs := []string{
		filepath.Join(workspace, "home"),
		filepath.Join(workspace, "tmp"),
		filepath.Join(workspace, "work"),
		filepath.Join(workspace, "runner"),
		filepath.Join(workspace, "homebrew", "bin"),
		filepath.Join(workspace, "keychain"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("creating workspace dir %s: %w", d, err)
		}
	}

	return &Runner{
		cfg:       cfg,
		workspace: workspace,
		done:      make(chan struct{}),
	}, nil
}

// Start extracts the runner, sets up Homebrew symlinks, creates a per-job
// keychain, generates a sandbox profile, and launches the runner inside
// sandbox-exec with its own process group.
func (r *Runner) Start(ctx context.Context) error {
	log := r.cfg.Log.With("job_id", r.cfg.JobID)

	// Copy runner to per-job directory
	runnerDst := filepath.Join(r.workspace, "runner")
	cpCmd := exec.CommandContext(ctx, "cp", "-a", r.cfg.RunnerDir+"/.", runnerDst+"/")
	if out, err := cpCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copying runner: %w (output: %s)", err, string(out))
	}

	// Set up Homebrew overlay: symlink executables from system Homebrew
	if err := r.setupHomebrewOverlay(); err != nil {
		log.Warn("homebrew overlay setup failed, continuing without it", "error", err)
	}

	// Create per-job keychain
	keychainPath := filepath.Join(r.workspace, "keychain", "job.keychain-db")
	if err := r.createKeychain(keychainPath); err != nil {
		log.Warn("keychain creation failed, continuing without it", "error", err)
	}

	// Generate sandbox profile
	profilePath := filepath.Join(r.workspace, "sandbox.sb")
	profile := GenerateSandboxProfile(r.workspace, r.cfg.DataDir)
	if err := os.WriteFile(profilePath, []byte(profile), 0o644); err != nil {
		return fmt.Errorf("writing sandbox profile: %w", err)
	}

	// Build the runner command
	runScript := filepath.Join(runnerDst, "run.sh")
	r.cmd = exec.CommandContext(ctx,
		"sandbox-exec", "-f", profilePath,
		runScript, "--jitconfig", r.cfg.JITConfig,
	)
	r.cmd.Dir = runnerDst
	r.cmd.Env = r.buildEnv(keychainPath)
	r.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Log file for runner output
	logPath := filepath.Join(r.workspace, "runner.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("creating runner log: %w", err)
	}
	r.cmd.Stdout = logFile
	r.cmd.Stderr = logFile

	if err := r.cmd.Start(); err != nil {
		if closeErr := logFile.Close(); closeErr != nil {
			log.Warn("closing log file after start failure", "error", closeErr)
		}
		return fmt.Errorf("starting runner: %w", err)
	}

	log.Info("native macOS runner started", "pid", r.cmd.Process.Pid, "workspace", r.workspace)

	// Background goroutine to wait for process exit
	go func() {
		defer close(r.done)
		defer func() {
			if err := logFile.Close(); err != nil {
				log.Warn("closing runner log file", "error", err)
			}
		}()
		r.waitErr = r.cmd.Wait()
		if r.waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(r.waitErr, &exitErr) {
				r.exitCode = exitErr.ExitCode()
			} else {
				r.exitCode = -1
			}
		}
	}()

	return nil
}

// Wait blocks until the runner process exits and returns the exit code.
func (r *Runner) Wait(_ context.Context) (int, error) {
	<-r.done
	return r.exitCode, r.waitErr
}

// Stop kills the runner process group, deletes the per-job keychain,
// and removes the workspace directory.
func (r *Runner) Stop() {
	log := r.cfg.Log.With("job_id", r.cfg.JobID)

	// Kill the process group
	if r.cmd != nil && r.cmd.Process != nil {
		pgid := r.cmd.Process.Pid
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			log.Debug("killing process group", "pgid", pgid, "error", err)
		}
	}

	// Wait for the process to actually exit
	<-r.done

	// Delete per-job keychain
	keychainPath := filepath.Join(r.workspace, "keychain", "job.keychain-db")
	deleteCmd := exec.Command("security", "delete-keychain", keychainPath)
	if out, err := deleteCmd.CombinedOutput(); err != nil {
		log.Debug("deleting keychain", "error", err, "output", string(out))
	}

	// Remove workspace
	if err := os.RemoveAll(r.workspace); err != nil {
		log.Warn("removing workspace", "error", err)
	}
}

// Address returns the runner's network address. Native runners run locally
// so there is no separate IP address.
func (r *Runner) Address() string {
	return ""
}

// setupHomebrewOverlay symlinks executables from /opt/homebrew/bin into the
// job's isolated Homebrew bin directory.
func (r *Runner) setupHomebrewOverlay() error {
	systemBin := "/opt/homebrew/bin"
	jobBin := filepath.Join(r.workspace, "homebrew", "bin")

	entries, err := os.ReadDir(systemBin)
	if err != nil {
		return fmt.Errorf("reading %s: %w", systemBin, err)
	}

	for _, e := range entries {
		src := filepath.Join(systemBin, e.Name())
		dst := filepath.Join(jobBin, e.Name())
		if err := os.Symlink(src, dst); err != nil && !os.IsExist(err) {
			// Non-fatal: skip individual symlink failures
			continue
		}
	}

	return nil
}

// createKeychain creates a per-job macOS keychain.
func (r *Runner) createKeychain(path string) error {
	// Create the keychain with an empty password (isolated, per-job)
	createCmd := exec.Command("security", "create-keychain", "-p", "", path)
	if out, err := createCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("security create-keychain: %w (output: %s)", err, string(out))
	}

	// Set it as the default keychain for this job's environment
	defaultCmd := exec.Command("security", "default-keychain", "-s", path)
	if out, err := defaultCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("security default-keychain: %w (output: %s)", err, string(out))
	}

	// Unlock the keychain so the runner can use it
	unlockCmd := exec.Command("security", "unlock-keychain", "-p", "", path)
	if out, err := unlockCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("security unlock-keychain: %w (output: %s)", err, string(out))
	}

	return nil
}

// buildEnv constructs the environment variables for the runner process.
func (r *Runner) buildEnv(keychainPath string) []string {
	homeDir := filepath.Join(r.workspace, "home")
	tmpDir := filepath.Join(r.workspace, "tmp")
	workDir := filepath.Join(r.workspace, "work")
	brewDir := filepath.Join(r.workspace, "homebrew")

	env := []string{
		"HOME=" + homeDir,
		"TMPDIR=" + tmpDir,
		"RUNNER_TEMP=" + tmpDir,
		"RUNNER_WORK=" + workDir,
		"RUNNER_TOOL_CACHE=" + filepath.Join(homeDir, "tool-cache"),
		"HOMEBREW_PREFIX=" + brewDir,
		"HOMEBREW_CELLAR=" + filepath.Join(brewDir, "Cellar"),
		"HOMEBREW_TEMP=" + tmpDir,
		"PATH=" + filepath.Join(brewDir, "bin") + ":" + "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=en_US.UTF-8",
	}

	// Add keychain to login search list
	if keychainPath != "" {
		env = append(env, "EPHEMERD_KEYCHAIN="+keychainPath)
	}

	return env
}

// GenerateSandboxProfile creates a macOS sandbox-exec profile (.sb) for a job.
// The profile:
//   - Denies outbound connections to RFC1918 private networks
//   - Denies network-bind (the runner should not listen)
//   - Denies writes to system directories (/opt/homebrew, /Applications, /usr/local)
//   - Denies reads of sensitive ephemerd files (.ssh, config.toml, socket)
//   - Allows reads/writes within the job workspace
func GenerateSandboxProfile(workspace, dataDir string) string {
	var sb strings.Builder

	sb.WriteString("(version 1)\n")
	sb.WriteString("(allow default)\n\n")

	// Deny outbound connections to RFC1918 / link-local ranges
	sb.WriteString("; Block private network access\n")
	sb.WriteString("(deny network-outbound (remote ip \"10.0.0.0/8\"))\n")
	sb.WriteString("(deny network-outbound (remote ip \"172.16.0.0/12\"))\n")
	sb.WriteString("(deny network-outbound (remote ip \"192.168.0.0/16\"))\n")
	sb.WriteString("(deny network-outbound (remote ip \"169.254.0.0/16\"))\n\n")

	// Deny bind — runner should not listen on any port
	sb.WriteString("; Deny network listening\n")
	sb.WriteString("(deny network-bind)\n\n")

	// Deny writes to system directories
	sb.WriteString("; Deny writes to system paths\n")
	sb.WriteString("(deny file-write* (subpath \"/opt/homebrew\"))\n")
	sb.WriteString("(deny file-write* (subpath \"/Applications\"))\n")
	sb.WriteString("(deny file-write* (subpath \"/usr/local\"))\n\n")

	// Deny reads of sensitive ephemerd files
	sb.WriteString("; Deny access to sensitive files\n")
	homeDir, err := os.UserHomeDir()
	if err == nil {
		fmt.Fprintf(&sb, "(deny file-read* (subpath \"%s/.ssh\"))\n", homeDir)
	}
	fmt.Fprintf(&sb, "(deny file-read* (literal \"%s/config.toml\"))\n", dataDir)
	fmt.Fprintf(&sb, "(deny file-read* (literal \"%s/ephemerd.sock\"))\n", dataDir)
	sb.WriteString("\n")

	// Allow reads/writes within the job workspace
	sb.WriteString("; Allow job workspace access\n")
	fmt.Fprintf(&sb, "(allow file-read* (subpath \"%s\"))\n", workspace)
	fmt.Fprintf(&sb, "(allow file-write* (subpath \"%s\"))\n", workspace)

	return sb.String()
}
