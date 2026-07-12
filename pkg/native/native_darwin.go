//go:build darwin

package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// serviceUserMu serializes service user creation across concurrent job starts.
var serviceUserMu sync.Mutex

// ServiceUserName is the hidden macOS service account that native runner
// jobs execute as when no [runner.macos] user is configured. It is created
// lazily on first use and persists like other service accounts (_www, ...).
// Per-job user deletion is deliberately avoided: dscl/sysadminctl user
// deletion wedges opendirectoryd on modern macOS.
const ServiceUserName = "_ephemerd"

// ServiceGroupName is a dedicated primary group for the service user.
// Using a dedicated group instead of staff (gid 20 — the default group for
// every normal macOS account) keeps the runner process from inheriting
// group access to the many files on a typical Mac that are staff-group
// owned. Falls back to staff if the group can't be created.
const ServiceGroupName = "_ephemerd"

// staffGID is the macOS staff group, used as the fallback primary group
// when a dedicated service group can't be provisioned.
const staffGID = 20

// service{UID,GID} ranges are scanned for a free id when creating the
// service user/group. macOS reserves <500 for system accounts; 600-999
// is the conventional band for added service accounts.
const (
	serviceUIDMin = 600
	serviceUIDMax = 999
)

// ensureServiceUser creates the _ephemerd service user if it doesn't exist
// and returns its credential.
func (r *Runner) ensureServiceUser() (*syscall.Credential, error) {
	serviceUserMu.Lock()
	defer serviceUserMu.Unlock()

	// Already exists?
	if cred, err := lookupCredential(ServiceUserName); err == nil {
		return cred, nil
	}

	// Find a free UID
	out, err := exec.Command("dscl", ".", "-list", "/Users", "UniqueID").Output()
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	used := make(map[int]bool)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			if id, err := strconv.Atoi(fields[1]); err == nil {
				used[id] = true
			}
		}
	}
	uid := 0
	for id := serviceUIDMin; id <= serviceUIDMax; id++ {
		if !used[id] {
			uid = id
			break
		}
	}
	if uid == 0 {
		return nil, fmt.Errorf("no free UID in range %d-%d", serviceUIDMin, serviceUIDMax)
	}

	// Resolve a dedicated primary group, falling back to staff (gid 20)
	// if provisioning fails for any reason — that's the previously-tested
	// behavior, so a group hiccup never blocks native jobs.
	gid := r.ensureServiceGroup()

	// NFSHomeDirectory is /var/empty (like _www and other service
	// accounts). Registering a real directory as a user home puts it
	// under macOS data protection — even root then can't delete it
	// without Full Disk Access. The runner's HOME env var points at the
	// per-job dir; the DS record never needs to.
	steps := [][]string{
		{"dscl", ".", "-create", "/Users/" + ServiceUserName},
		{"dscl", ".", "-create", "/Users/" + ServiceUserName, "UserShell", "/bin/bash"},
		{"dscl", ".", "-create", "/Users/" + ServiceUserName, "UniqueID", strconv.Itoa(uid)},
		{"dscl", ".", "-create", "/Users/" + ServiceUserName, "PrimaryGroupID", strconv.Itoa(gid)},
		{"dscl", ".", "-create", "/Users/" + ServiceUserName, "NFSHomeDirectory", "/var/empty"},
		{"dscl", ".", "-create", "/Users/" + ServiceUserName, "IsHidden", "1"},
	}
	for _, args := range steps {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("%v: %s: %w", args, strings.TrimSpace(string(out)), err)
		}
	}
	r.log.Info("created ephemerd service user", "user", ServiceUserName, "uid", uid, "gid", gid)

	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}

// ensureServiceGroup returns the gid of a dedicated _ephemerd primary
// group, creating it if needed. On any failure it logs a warning and
// returns staffGID (20) so native jobs keep working with the previously
// tested behavior. Caller holds serviceUserMu.
func (r *Runner) ensureServiceGroup() int {
	if g, err := user.LookupGroup(ServiceGroupName); err == nil {
		if gid, perr := strconv.Atoi(g.Gid); perr == nil {
			return gid
		}
	}

	out, err := exec.Command("dscl", ".", "-list", "/Groups", "PrimaryGroupID").Output()
	if err != nil {
		r.log.Warn("listing groups for service group; falling back to staff", "error", err)
		return staffGID
	}
	used := make(map[int]bool)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			if id, perr := strconv.Atoi(fields[1]); perr == nil {
				used[id] = true
			}
		}
	}
	gid := 0
	for id := serviceUIDMin; id <= serviceUIDMax; id++ {
		if !used[id] {
			gid = id
			break
		}
	}
	if gid == 0 {
		r.log.Warn("no free GID for service group; falling back to staff", "range", fmt.Sprintf("%d-%d", serviceUIDMin, serviceUIDMax))
		return staffGID
	}

	steps := [][]string{
		{"dscl", ".", "-create", "/Groups/" + ServiceGroupName},
		{"dscl", ".", "-create", "/Groups/" + ServiceGroupName, "PrimaryGroupID", strconv.Itoa(gid)},
		{"dscl", ".", "-create", "/Groups/" + ServiceGroupName, "RealName", "ephemerd native runners"},
	}
	for _, args := range steps {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			r.log.Warn("creating service group; falling back to staff",
				"step", strings.Join(args, " "), "output", strings.TrimSpace(string(out)), "error", err)
			return staffGID
		}
	}
	r.log.Info("created ephemerd service group", "group", ServiceGroupName, "gid", gid)
	return gid
}

// lookupCredential resolves a username to a syscall.Credential for
// privilege dropping via SysProcAttr.
func lookupCredential(username string) (*syscall.Credential, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return nil, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parsing uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parsing gid %q: %w", u.Gid, err)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}

// defaultMaxProcesses is the fork-bomb ulimit -u applied to native jobs when
// the operator does not override it. Generous on purpose: clang, php, and
// parallel build tools legitimately fork hundreds of children, but 2048 still
// stops a runaway fork bomb from exhausting the host process table.
const defaultMaxProcesses = 2048

// Runner executes a GitHub Actions runner directly on the macOS host
// inside a per-job sandbox. Each job gets its own workspace, HOME,
// TMPDIR, keychain, and Homebrew prefix.
type Runner struct {
	dataDir   string
	jobID     string
	jitConfig string
	runnerSrc string // path to extracted GHA runner (runner.Manager.Dir())
	log       *slog.Logger

	jobDir        string // <dataDir>/native/<jobID>/
	pemPath       string // configured GitHub App private_key_path (empty if unset / PAT auth)
	keychainPath  string // per-job keychain
	runAsUser     string // existing user to run as (empty = _ephemerd service user)
	jobUID        uint32 // uid the runner executes as
	sandboxStrict bool   // deny-by-default sandbox profile (opt-in)
	maxProcesses  int    // ulimit -u for the job (0 = unlimited)
	cmd           *exec.Cmd
	pgid          int
}

// SetSandboxStrict enables the deny-by-default sandbox profile. Opt-in: the
// strict allow-list is not guaranteed to cover every toolchain and must be
// live-smoke-tested before it is turned on for a host.
func (r *Runner) SetSandboxStrict(strict bool) {
	r.sandboxStrict = strict
}

// SetMaxProcesses sets the ulimit -u process cap applied before the runner is
// exec'd. 0 disables the cap (unlimited).
func (r *Runner) SetMaxProcesses(n int) {
	r.maxProcesses = n
}

// SetRunAsUser configures a non-root user to run the runner process as.
// The daemon (running as root) drops privileges via setuid/setgid when
// launching the runner. Strongly recommended when the daemon runs as root:
// without it, CI job steps execute as root on the host.
func (r *Runner) SetRunAsUser(username string) {
	r.runAsUser = username
}

// New creates a native macOS runner for a single job. It prepares the
// workspace directory structure but does not start the runner process.
//
// pemPath is the configured GitHub App private_key_path (empty for PAT
// auth). It is threaded into the sandbox profile so the runner is denied
// read access to the operator's App private key even though the daemon
// runs as root with no HOME — see GenerateSandboxProfile.
func New(dataDir, jobID, jitConfig, runnerSrc, pemPath string, log *slog.Logger) (*Runner, error) {
	jobDir := filepath.Join(dataDir, "native", jobID)

	// Create workspace directories
	dirs := []string{
		filepath.Join(jobDir, "home"),
		filepath.Join(jobDir, "tmp"),
		filepath.Join(jobDir, "work"),
		filepath.Join(jobDir, "runner"),
		filepath.Join(jobDir, "keychain"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("creating directory %s: %w", d, err)
		}
	}

	return &Runner{
		dataDir:      dataDir,
		jobID:        jobID,
		jitConfig:    jitConfig,
		runnerSrc:    runnerSrc,
		pemPath:      pemPath,
		log:          log,
		jobDir:       jobDir,
		maxProcesses: defaultMaxProcesses,
	}, nil
}

// Start copies the runner binary, sets up the sandbox and environment,
// and launches the runner process.
func (r *Runner) Start(ctx context.Context) error {
	runnerDir := filepath.Join(r.jobDir, "runner")

	// Copy runner files from the extracted source (hard link, fall back to copy)
	if err := copyRunnerFiles(r.runnerSrc, runnerDir); err != nil {
		return fmt.Errorf("copying runner files: %w", err)
	}

	// Use the host's real Homebrew (read-only: the sandbox denies writes to
	// /opt/homebrew). Pointing HOMEBREW_PREFIX/CELLAR at a per-job empty prefix
	// made `brew list`-style checks (e.g. spc doctor) see nothing installed, so
	// tools tried to (re)install formulae the host already has — then failed on
	// the write-deny. Sharing the host prefix read-only lets jobs USE the
	// pre-installed deps without being able to mutate them.
	const hostBrewPrefix = "/opt/homebrew"

	// Resolve the host's active developer directory (full Xcode or Command
	// Line Tools) up-front: strict-mode needs it in the allow-list, and the
	// runner needs it in DEVELOPER_DIR. Hardcoding the Xcode.app path breaks
	// xcrun shims (git, clang) on hosts with only CLT installed.
	developerDir := ""
	if devDir, err := exec.Command("xcode-select", "-p").Output(); err == nil {
		developerDir = strings.TrimSpace(string(devDir))
	}

	// Generate and write sandbox profile
	profilePath := filepath.Join(r.jobDir, "sandbox.sb")
	profile := GenerateSandboxProfile(r.jobDir, r.dataDir, r.pemPath, SandboxOptions{
		Strict:         r.sandboxStrict,
		HomebrewPrefix: hostBrewPrefix,
		DeveloperDir:   developerDir,
	})
	if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
		return fmt.Errorf("writing sandbox profile: %w", err)
	}

	// Set up per-job keychain
	r.keychainPath = filepath.Join(r.jobDir, "keychain", "job.keychain-db")
	if err := r.createKeychain(); err != nil {
		r.log.Warn("failed to create per-job keychain", "error", err)
		// Non-fatal: jobs that don't need signing will work fine
	}

	// Build environment
	homeDir := filepath.Join(r.jobDir, "home")
	tmpDir := filepath.Join(r.jobDir, "tmp")
	workDir := filepath.Join(r.jobDir, "work")

	env := []string{
		"HOME=" + homeDir,
		"TMPDIR=" + tmpDir,
		"RUNNER_WORK_FOLDER=" + workDir,
		"PATH=" + hostBrewPrefix + "/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOMEBREW_PREFIX=" + hostBrewPrefix,
		"HOMEBREW_CELLAR=" + hostBrewPrefix + "/Cellar",
		"HOMEBREW_TEMP=" + tmpDir,
		"LANG=en_US.UTF-8",
	}
	if developerDir != "" {
		env = append(env, "DEVELOPER_DIR="+developerDir)
	}
	if r.keychainPath != "" {
		env = append(env, "EPHEMERD_KEYCHAIN="+r.keychainPath)
	}

	// Launch via sandbox-exec for filesystem/network isolation, wrapped in a
	// shell that applies resource limits (ulimit) BEFORE exec so they bind the
	// runner and its whole process tree. macOS has no cgroups, so this is the
	// only fork-bomb/CPU defense available on the native path; RAM and disk
	// still cannot be hard-capped here (use the VM path for that).
	launchName, launchArgs := buildLaunchArgs(profilePath, r.jitConfig, r.maxProcesses)
	r.cmd = exec.CommandContext(ctx, launchName, launchArgs...)
	r.cmd.Dir = runnerDir
	r.cmd.Env = env

	// Own process group for clean kill
	r.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Drop privileges. Job steps must never run as root on the host:
	//   - user configured: run as that existing user
	//   - no user configured + daemon is root: run as the hidden _ephemerd
	//     service user (created lazily on first use)
	//   - daemon not root: run as the daemon's own user (no setuid possible)
	var cred *syscall.Credential
	username := r.runAsUser
	switch {
	case r.runAsUser != "":
		c, err := lookupCredential(r.runAsUser)
		if err != nil {
			return fmt.Errorf("looking up run-as user %q: %w", r.runAsUser, err)
		}
		cred = c
	case os.Geteuid() == 0:
		c, err := r.ensureServiceUser()
		if err != nil {
			return fmt.Errorf("ensuring service user: %w", err)
		}
		username = ServiceUserName
		cred = c
	}
	if cred != nil {
		if out, err := exec.Command("chown", "-R",
			fmt.Sprintf("%d:%d", cred.Uid, cred.Gid), r.jobDir).CombinedOutput(); err != nil {
			return fmt.Errorf("chowning job dir to %s: %s: %w", username, strings.TrimSpace(string(out)), err)
		}
		r.cmd.SysProcAttr.Credential = cred
		r.jobUID = cred.Uid
		env = append(env, "USER="+username, "LOGNAME="+username)
		r.cmd.Env = env
	}

	// Log to files in the job directory (after chown so the runner user owns it)
	logPath := filepath.Join(r.jobDir, "runner.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("creating log file: %w", err)
	}
	r.cmd.Stdout = logFile
	r.cmd.Stderr = logFile

	if err := r.cmd.Start(); err != nil {
		if closeErr := logFile.Close(); closeErr != nil {
			r.log.Warn("failed to close log file", "error", closeErr)
		}
		return fmt.Errorf("starting runner: %w", err)
	}

	r.pgid = r.cmd.Process.Pid
	r.log.Info("native macOS runner started",
		"job_id", r.jobID,
		"pid", r.pgid,
		"dir", runnerDir,
	)

	return nil
}

// Wait blocks until the runner process exits and returns its exit code.
func (r *Runner) Wait() (int, error) {
	if r.cmd == nil || r.cmd.Process == nil {
		return -1, fmt.Errorf("runner not started")
	}

	err := r.cmd.Wait()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return -1, fmt.Errorf("waiting for runner: %w", err)
	}
	return 0, nil
}

// Stop forcefully terminates the runner and all its children, cleans up
// the keychain, and removes the job workspace.
func (r *Runner) Stop() {
	// Kill the process group
	if r.pgid > 0 {
		if err := syscall.Kill(-r.pgid, syscall.SIGKILL); err != nil {
			// Process may have already exited — not an error
			r.log.Debug("kill process group", "pgid", r.pgid, "error", err)
		}

		// Fallback: kill any orphaned children
		cmd := exec.Command("pkill", "-9", "-P", strconv.Itoa(r.pgid))
		if err := cmd.Run(); err != nil {
			r.log.Debug("pkill fallback", "ppid", r.pgid, "error", err)
		}
	}

	// Delete per-job keychain
	if r.keychainPath != "" {
		r.deleteKeychain()
	}

	// Note: no per-UID process kill here — the service user is shared
	// across concurrent jobs, so pkill -U would kill other jobs' steps.
	// The pgid kill above covers the job's process tree.

	// Strip ACLs before removal: macOS frameworks put "deny delete" ACLs
	// on auto-created home subdirectories (~/Library etc.) which block
	// os.RemoveAll even as root.
	if out, err := exec.Command("chmod", "-RN", r.jobDir).CombinedOutput(); err != nil {
		r.log.Debug("stripping ACLs from job dir", "dir", r.jobDir,
			"output", strings.TrimSpace(string(out)), "error", err)
	}

	// Remove job workspace
	if err := os.RemoveAll(r.jobDir); err != nil {
		r.log.Warn("failed to remove job directory", "dir", r.jobDir, "error", err)
	}

	r.log.Info("native macOS runner cleaned up", "job_id", r.jobID)
}

// createKeychain creates a per-job temporary keychain.
func (r *Runner) createKeychain() error {
	cmd := exec.Command("security", "create-keychain", "-p", "", r.keychainPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create-keychain: %s: %w", strings.TrimSpace(string(out)), err)
	}
	cmd = exec.Command("security", "unlock-keychain", "-p", "", r.keychainPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("unlock-keychain: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// deleteKeychain removes the per-job keychain.
func (r *Runner) deleteKeychain() {
	cmd := exec.Command("security", "delete-keychain", r.keychainPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.log.Warn("failed to delete keychain", "path", r.keychainPath, "output", strings.TrimSpace(string(out)), "error", err)
	}
}

// SandboxOptions carries the knobs GenerateSandboxProfile needs beyond the
// job/data/pem paths.
type SandboxOptions struct {
	// Strict switches from allow-by-default (deny-list) to deny-by-default
	// (allow-list). Opt-in; default false keeps today's already-smoke-tested
	// allow-by-default profile byte-for-byte identical.
	Strict bool
	// HomebrewPrefix is the host Homebrew prefix (e.g. /opt/homebrew) the
	// runner uses read-only. Needed in the strict allow-list.
	HomebrewPrefix string
	// DeveloperDir is the resolved active Xcode / CLT path (xcode-select -p).
	// Needed in the strict allow-list so clang/git shims work. May be empty.
	DeveloperDir string
}

// GenerateSandboxProfile returns a macOS sandbox profile that restricts
// the runner process. Paths are templated with the job-specific directories.
//
// pemPath is the configured GitHub App private_key_path (may be empty).
// When set it is denied explicitly as a belt-and-suspenders measure on top
// of the broad /Users content deny below.
//
// Two postures:
//   - opts.Strict == false (default): allow-by-default with an explicit
//     deny-list. This is the previously-smoke-tested behavior and is emitted
//     byte-for-byte as before when strict is off.
//   - opts.Strict == true (opt-in): deny-by-default with an explicit
//     allow-list for the paths/services a GHA runner + toolchain needs, with
//     the same tier-1/tier-2 denies layered on top. Much stronger, but not
//     guaranteed complete for every toolchain — must be live-tested.
func GenerateSandboxProfile(jobDir, dataDir, pemPath string, opts SandboxOptions) string {
	// Resolve to absolute, symlink-free paths. The sandbox matches against
	// kernel (resolved) paths: /var and /tmp are symlinks to /private/var
	// and /private/tmp on macOS, so rules written with the unresolved
	// config paths (e.g. /var/lib/ephemerd/...) silently never match.
	resolve := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			return real
		}
		return abs
	}
	absJobDir := resolve(jobDir)
	absDataDir := resolve(dataDir)
	homeDir := resolve(os.Getenv("HOME"))

	// Belt-and-suspenders deny for the configured GitHub App private key.
	// The broad /Users content deny below already covers a key under any
	// user's home, but the PEM can live outside /Users (e.g. /etc,
	// /var/lib/ephemerd) so name it — and its parent dir — explicitly.
	// Resolved through the same helper so symlinked config paths match the
	// kernel's view.
	var pemDenies string
	if pemPath != "" {
		absPem := resolve(pemPath)
		pemDenies = fmt.Sprintf(`
;; === GitHub App private key (belt and suspenders) ===
(deny file-read* (literal "%[1]s"))
(deny file-read* (subpath "%[2]s"))
`, absPem, filepath.Dir(absPem))
	}

	// === Preamble: (allow default) vs (deny default) + strict allow-list ===
	//
	// The tier-1/tier-2 DENIES below apply in BOTH postures. In strict mode
	// they are redundant with (deny default) for most paths but still
	// meaningful: they win over the broad strict allows (e.g. read+exec of
	// /usr), so a secret that lives under an otherwise-allowed prefix stays
	// denied.
	var preamble string
	if opts.Strict {
		preamble = strictPreamble(absJobDir, opts)
	} else {
		preamble = "(allow default)"
	}

	return fmt.Sprintf(`(version 1)
%[5]s

;; === Network isolation ===
;; Note: sandbox-exec does not support CIDR notation for IP addresses.
;; Private network blocking (10.x, 172.16.x, 192.168.x) requires pf
;; firewall rules — handled separately. The sandbox blocks localhost
;; and port binding to prevent inter-job communication.

;; Allow DNS before blocking localhost (macOS resolves via mDNSResponder on 127.0.0.1)
(allow network-outbound (remote udp "localhost:53"))
(allow network-outbound (remote tcp "localhost:53"))

;; Block outbound to localhost (daemon control socket, other jobs)
(deny network-outbound (remote ip "localhost:*"))

;; Block binding to any port — prevents jobs from running servers
(deny network-bind (local ip "*:*"))

;; === Process isolation (NATIVE-3, tier-2) ===
;; Block this job from inspecting OTHER processes' state (argv, env,
;; fds) via libproc / proc_pidinfo. Every native job runs as the same
;; _ephemerd uid, and the JIT registration credential is passed on
;; run.sh's argv, so without this a sibling job could read it via ps or
;; a proc_pidinfo call. process-info* on (target others) blocks that
;; path; a job inspecting its OWN process tree (wait/kill/signal, and
;; proc_pidinfo on self) is unaffected.
;;
;; RESIDUAL — read the PR body: this does NOT close the argv leak
;; completely. On macOS the KERN_PROCARGS2 sysctl returns another pid's
;; argv and is NOT mediated by sandbox-exec (verified: neither
;; process-info* nor a blanket sysctl-read deny stops it, and a blanket
;; sysctl-read deny additionally breaks CPU-count detection). An
;; in-sandbox reader can therefore still recover a sibling's argv via
;; KERN_PROCARGS2, and a root/unsandboxed observer always can. The
;; complete fix is not passing the credential on argv (blocked upstream:
;; the GHA runner only accepts --jitconfig inline) or the VM path. This
;; rule still closes the common ps/libproc vector at zero cost to builds.
(deny process-info* (target others))

;; === Filesystem isolation ===

;; Isolate this job from sibling jobs and ephemerd internal state.
;; All native job workspaces live under <dataDir>/native/<jobID>, and
;; every native job runs as the same _ephemerd uid, so without this a
;; job could read a concurrent job's checkout token or source.
;;
;; Deny file-read-DATA (not file-read*) on the native subtree: on a
;; directory that blocks readdir (can't list a sibling's contents), on a
;; file it blocks reading contents. file-read-metadata stays allowed so
;; lstat/realpath path resolution can traverse THROUGH native/ — denying
;; metadata breaks the .NET host with "Failed to resolve full path of the
;; current executable" (exit 133).
(deny file-read-data (subpath "%[2]s/native"))
(deny file-write* (subpath "%[2]s/native"))

;; Re-allow reading the native directory NODE itself (not its children).
;; getcwd() and bash walk UP from the job's runner dir and must readdir
;; native/ to learn the job-id component name; without this they fail
;; with "getcwd: cannot access parent directories" and run.sh won't exec.
;; This leaks the list of concurrent job-id directory names (not their
;; contents) — job ids are not secret.
(allow file-read-data (literal "%[2]s/native"))

;; Block reading the CONTENTS of every user's home under /Users. The
;; daemon runs as root with no HOME, so a deny anchored on $HOME (below)
;; targets "/.ssh" and leaves the operator's real ~/.ssh — including the
;; GitHub App PEM and every other dotfile secret — readable under
;; (allow default). This closes that hole host-wide without depending on
;; the daemon's HOME.
;;
;; Deny file-read-DATA (not file-read*), mirroring the native/ subtree
;; technique above: on a file it blocks reading contents; on a directory
;; it blocks readdir. file-read-METADATA stays allowed so lstat/realpath
;; path resolution can traverse THROUGH /Users (denying metadata breaks
;; getcwd and the .NET host's executable-path resolution).
(deny file-read-data (subpath "/Users"))
(deny file-write* (subpath "/Users"))

;; Re-allow reading the /Users directory NODE itself (not its children),
;; same reason as the native/ node re-allow: getcwd()/bash walk UP through
;; /Users and must readdir it to resolve the current path. This leaks the
;; list of usernames (not their file contents) — usernames are not secret.
(allow file-read-data (literal "/Users"))

;; Block sensitive host paths entirely — read and write. .ssh was
;; previously read-only-denied, leaving a writable authorized_keys hole
;; on any host where the runner uid can reach the operator's home.
(deny file-read* (subpath "%[1]s/.ssh"))
(deny file-write* (subpath "%[1]s/.ssh"))
(deny file-read* (literal "%[2]s/config.toml"))
(deny file-write* (literal "%[2]s/config.toml"))
(deny file-read* (literal "%[2]s/ephemerd.sock"))
(deny file-write* (literal "%[2]s/ephemerd.sock"))
(deny file-read* (subpath "%[2]s/vm"))
(deny file-write* (subpath "%[2]s/vm"))
%[4]s
;; Block writes to shared tools (read-only access only)
(deny file-write* (subpath "/opt/homebrew"))
(deny file-write* (subpath "/Applications"))
(deny file-write* (subpath "/usr/local"))

;; Re-allow this job's own workspace (read + write). The explicit
;; file-read-data is required IN ADDITION to file-read*: macOS sandbox
;; resolves a specific-operation deny (the file-read-data deny on the
;; native subtree above) over a later wildcard allow (file-read*), so the
;; read-data re-allow must name the operation explicitly to win for this
;; job's own files.
;;
;; NATIVE-5 (tier-2): the world-shared /private/tmp write allow was
;; removed. Jobs get TMPDIR=<jobDir>/tmp (under this subtree), so scratch
;; writes still land in the per-job workspace and the cross-job /tmp
;; channel is closed. Tools that hardcode /tmp instead of honoring TMPDIR
;; will now be denied there — flagged for a live smoke test.
(allow file-read* (subpath "%[3]s"))
(allow file-read-data (subpath "%[3]s"))
(allow file-write* (subpath "%[3]s"))
`, homeDir, absDataDir, absJobDir, pemDenies, preamble)
}

// strictPreamble builds the deny-by-default header and the explicit
// allow-list a GHA runner + typical toolchain needs. It is only used when
// SandboxOptions.Strict is true. The tier-1/tier-2 DENIES in the main body
// still layer on top of these allows (a specific-operation deny wins over a
// broad allow), so job-to-job and job-to-daemon holes stay closed.
//
// This allow-list is a best-effort starting point, NOT a proven-complete
// set — that is precisely why strict mode is opt-in and default-off. Expect
// to add entries after live smoke tests (missing mach services, sysctls, or
// dylib paths surface as sandbox denials in the system log).
func strictPreamble(absJobDir string, opts SandboxOptions) string {
	brew := opts.HomebrewPrefix
	if brew == "" {
		brew = "/opt/homebrew"
	}
	// Developer dir allow only if resolved; an empty (subpath "") would
	// match everything and silently defeat deny-by-default.
	var devAllow string
	if opts.DeveloperDir != "" {
		devAllow = fmt.Sprintf("(allow file-read* process-exec (subpath \"%s\"))\n", opts.DeveloperDir)
	}
	return fmt.Sprintf(`;; === STRICT MODE: deny-by-default + explicit allow-list (opt-in) ===
;; Everything not explicitly allowed here (or re-allowed for the per-job
;; workspace below) is denied. This is a starting allow-list; add entries
;; after live smoke tests surface missing services in the system log.
(deny default)

;; Import Apple's system base profile. Under (deny default) a process
;; cannot even complete dyld startup without the mach/iokit/dyld-shared-
;; cache allows this bundle provides; without it /bin/echo SIGABRTs before
;; main(). Verified: importing system.sb lets sysctl, bash+fork, and file
;; writes run under deny-default (see PR body live-test notes).
(import "system.sb")

;; Core read+exec system trees the runner/toolchain load binaries and
;; dylibs from.
(allow file-read* process-exec
  (subpath "/usr")
  (subpath "/bin")
  (subpath "/sbin")
  (subpath "/System")
  (subpath "/Library")
  (subpath "/Applications")
  (subpath "/private/var/select")
  (subpath "/private/etc")
  (subpath "%[1]s"))
%[2]s
;; NOTE: use the RESOLVED /private/etc, not /etc. /etc is a symlink to
;; /private/etc and the sandbox matches kernel (resolved) paths, so an
;; "/etc" allow silently matches nothing — which broke curl/openssl
;; (fopen /private/etc/ssl/openssl.cnf -> "Operation not permitted").
;;
;; Toolchain per-user cache (DARWIN_USER_CACHE_DIR): clang, xcrun and
;; friends write their module/compile cache under /var/folders/...
;; (resolved /private/var/folders) regardless of TMPDIR. Without this,
;; clang fails with "couldn't create cache file" and compilation dies.
(allow file-read* file-write* (subpath "/private/var/folders"))

;; Per-job workspace: full read+write (home, tmp, work, runner, keychain,
;; and TMPDIR which lives under it). The main body re-allows this too; it
;; is repeated here so deny-default does not swallow it before those rules.
(allow file-read* file-write* (subpath "%[3]s"))

;; Process + fork so the runner can spawn job steps and toolchains.
(allow process-fork)
(allow process-exec)

;; Network egress (private ranges are blocked by pf, localhost by the
;; deny below in the main body).
(allow network-outbound)

;; Sysctls the Go/.NET runtimes and build tools read (CPU count, memory,
;; page size). NOT a blanket sysctl-read gap: the KERN_PROCARGS2 argv leak
;; is not mediated by sandbox-exec regardless (see NATIVE-3 note), so
;; allowing sysctl-read here does not widen the argv exposure; it only lets
;; hw.ncpu-style build detection work.
(allow sysctl-read)

;; Mach services the runner, clang, and DNS resolution look up. system.sb
;; already allows the core set; this is belt-and-suspenders for toolchains.
(allow mach-lookup)

;; POSIX IPC / shared memory used by toolchains.
(allow ipc-posix-shm)

;; Allow signalling our own process tree.
(allow signal (target self))
`, brew, devAllow, absJobDir)
}

// buildLaunchArgs constructs the command name + args for launching the
// runner. The launch is wrapped in a shell so a ulimit can be applied BEFORE
// exec, binding the runner and its whole process tree. maxProc is the
// ulimit -u value; 0 skips the limit (unlimited). No memory (-v) or CPU-time
// (-t) limit is set by default: macOS has no cgroups, so those are too easy
// to trip on legitimate heavy builds. This is fork-bomb/CPU defense only.
//
// Kept as a standalone function so the argv construction is unit-testable
// without launching a real sandbox-exec.
func buildLaunchArgs(profilePath, jitConfig string, maxProc int) (string, []string) {
	// Build the ulimit prefix. Values are integers (maxProc, from config) so
	// no shell-escaping of untrusted input is needed here; profilePath and
	// jitConfig are passed as separate exec args to sandbox-exec, not
	// interpolated into the shell string, so a jitConfig with shell
	// metacharacters cannot break out.
	var ulimitPrefix string
	if maxProc > 0 {
		ulimitPrefix = fmt.Sprintf("ulimit -u %d; ", maxProc)
	}
	script := ulimitPrefix + `exec sandbox-exec -f "$1" ./run.sh --jitconfig "$2"`
	// $0 is a label; "$1"/"$2" receive profilePath/jitConfig via argv so no
	// interpolation into the script body occurs.
	return "/bin/sh", []string{"-c", script, "ephemerd-native", profilePath, jitConfig}
}

// copyRunnerFiles copies the runner directory to the per-job location.
// Uses hard links for efficiency, falling back to full copy on error.
func copyRunnerFiles(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("computing relative path: %w", err)
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	sf, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening source %s: %w", src, err)
	}
	defer func() {
		if closeErr := sf.Close(); closeErr != nil {
			// Best-effort close; source is read-only
			_ = closeErr
		}
	}()

	info, err := sf.Stat()
	if err != nil {
		return fmt.Errorf("stat source %s: %w", src, err)
	}

	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return fmt.Errorf("creating dest %s: %w", dst, err)
	}

	if _, err := io.Copy(df, sf); err != nil {
		if closeErr := df.Close(); closeErr != nil {
			// Log would be ideal but we don't have a logger here
			_ = closeErr
		}
		return fmt.Errorf("copying %s → %s: %w", src, dst, err)
	}

	return df.Close()
}
