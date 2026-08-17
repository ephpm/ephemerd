// Package upgrade replaces the running ephemerd binary with a specific
// published release and restarts the service into it.
//
// The delivery model is "command, not bytes": a caller (the CLI or the
// control-plane Upgrade RPC) says "go to version vX.Y.Z" and the daemon
// downloads + checksum-verifies the release asset over its OWN outbound
// HTTPS — the same channel `install` and cloudflared already use. This is
// provider-agnostic and has no exec-channel size or timeout limit, unlike
// pushing an ~1 GB zip through a hypervisor guest-agent exec.
//
// Safety is staged: nothing touches the live binary until the new one is
// downloaded, checksum-verified, and (when natively runnable) probed with
// `--version`. The old binary is kept alongside as `<name>.old` for
// rollback. Any failure before the final swap leaves the node running the
// old binary; only swap+restart is the point of no easy return, and it
// happens last.
package upgrade

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// defaultRepoBase is the GitHub Releases download base. The full asset
	// URL is <defaultRepoBase>/<tag>/<asset>, with checksums.txt beside it.
	defaultRepoBase = "https://github.com/ephpm/ephemerd/releases/download"
	checksumsName   = "checksums.txt"

	defaultDrainTimeout = 45 * time.Minute
	defaultDrainPoll    = 5 * time.Second

	// restartDelay gives the caller's progress stream time to flush the
	// final RESTARTING message and disconnect cleanly before the service
	// goes down, so a mid-message connection reset can't be misread as a
	// failed upgrade.
	restartDelay = 2 * time.Second

	// restartWatchdog bounds how long we wait, after asking the service
	// manager to restart us, for that restart to actually take. A restart
	// that works kills this process, so still being alive when the watchdog
	// fires IS the failure signal — there is nothing else to observe.
	//
	// A successful restart is a stop plus a start; the daemon's own SCM stop
	// path checkpoints every 10s and the scheduler is already drained by
	// this point, so a healthy restart lands in seconds. 90s is generous
	// without leaving the node cordoned for long if it never comes.
	restartWatchdog = 90 * time.Second

	// restartRetryDelay is the shorter wait used when the restart request
	// itself failed outright (nothing was handed to the service manager, so
	// there is nothing to wait for).
	restartRetryDelay = 5 * time.Second

	// restartAttempts is how many times we ask for the restart before
	// declaring it failed and putting the node back into service. Retrying
	// is safe precisely because a restart that took effect would already
	// have terminated us.
	restartAttempts = 2

	// defaultStallTimeout is how long the release download may make NO
	// progress before it is abandoned.
	//
	// This is the one hole the rest of the cordon-safety machinery could not
	// close. Every *error* path un-cordons, but a download that neither
	// fails nor finishes produces no error to un-cordon on: the HTTP client
	// has no timeout by design (the asset is ~1 GB and a slow link is not a
	// failure), the only cancellation is the caller's context, and the
	// caller here is a hypervisor guest-agent exec that is never actually
	// killed when the operator's side gives up polling it. A blackholed TCP
	// connection therefore parks io.Copy forever with the scheduler
	// cordoned — a node that looks healthy and silently takes no work.
	//
	// A stall, unlike a slow transfer, is unambiguous: zero bytes for two
	// minutes on a connection that is supposed to be streaming means the
	// path is gone. Aborting turns the hang into an ordinary error, which
	// the existing defer un-cordons.
	defaultStallTimeout = 2 * time.Minute

	// defaultInstallTimeout bounds the whole cordoned-but-not-yet-swapped
	// window: download, checksum verify, extract and probe.
	//
	// The stall timeout above covers the failure mode we know about. This
	// covers the ones we do not — a verify or probe that wedges, a
	// pathologically slow but never-quite-stalled transfer, anything future
	// code adds between the cordon and the swap. It is a backstop, so it is
	// set well above any plausible healthy run: the fleet's slowest real
	// upgrade (a ~984 MB Windows zip) goes cordon-to-swap in about 40
	// seconds.
	//
	// It deliberately starts AFTER the drain wait, which has its own
	// (much longer, job-length) timeout and is not a hang when it is slow.
	defaultInstallTimeout = 30 * time.Minute
)

// versionRe matches release tags: vX.Y.Z with an optional prerelease
// suffix (v0.0.0-rc1). Mirrors the tag gate in .github/workflows/release.yml.
var versionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.]+)?$`)

// State is the coarse phase of an in-progress upgrade. It maps 1:1 to the
// apiv1.UpgradeState enum (translated in pkg/scheduler/grpc.go) so this
// package needn't import the generated protobuf types.
type State int

const (
	StatePreflight State = iota
	StateUpToDate
	StateDraining
	StateDownloading
	StateVerifying
	StateStaging
	StateSwapping
	StateRestarting
	StateFailed
)

// Progress is one observable step of an upgrade.
type Progress struct {
	State           State
	Message         string
	CurrentVersion  string
	TargetVersion   string
	ActiveJobs      int   // populated during StateDraining
	BytesDownloaded int64 // populated during StateDownloading
	BytesTotal      int64 // total asset size if the server reported it, else 0
}

// Emit receives progress updates. Implementations must not block for long;
// the RPC handler forwards each to a gRPC stream Send.
type Emit func(Progress)

// Drainer is the slice of the scheduler the upgrade needs: stop claiming
// new jobs, report how many are still running so we can wait for idle, and
// resume claiming if we abort before the restart. The scheduler's existing
// Cordon/Uncordon (added in #132) satisfy this; ActiveJobs is a thin
// accessor over the running-job map.
type Drainer interface {
	Cordon() int     // stop claiming new jobs; returns the current active count
	Uncordon() int   // resume claiming (used to back out an aborted upgrade)
	ActiveJobs() int // number of jobs currently running
}

// RunOptions configures a single upgrade. The exported override fields
// (InstallPath, Restart, Probe, GOOS, GOARCH, StageDir, HTTPClient) default
// to real behavior when zero and exist mainly so tests can inject seams.
type RunOptions struct {
	TargetVersion   string
	CurrentVersion  string
	BaseURLOverride string // replaces the release base dir URL; for mirrors/tests
	NoDrain         bool
	Force           bool
	DrainTimeout    time.Duration
	DrainPoll       time.Duration
	Drainer         Drainer
	Log             *slog.Logger

	// Shutdown, when non-nil, is closed (or is a ctx.Done()) as soon as the
	// daemon begins going down. It is how the restart supervisor learns that
	// the restart it asked for actually took effect: without it, a slow but
	// healthy stop would be misread as a failed restart and the node would be
	// un-cordoned on its way out the door. Optional; nil means the only
	// evidence of success is process death.
	Shutdown <-chan struct{}

	// StallTimeout abandons the download when it makes no progress for this
	// long. Zero means defaultStallTimeout; negative disables the check.
	StallTimeout time.Duration

	// InstallTimeout bounds everything between the end of the drain and the
	// binary swap. Zero means defaultInstallTimeout; negative disables it.
	InstallTimeout time.Duration

	// Test/override seams.
	InstallPath     string                            // default: resolved os.Executable()
	StageDir        string                            // default: <installdir>/.ephemerd-upgrade
	HTTPClient      *http.Client                      // default: http.DefaultClient (no timeout; ctx-governed)
	GOOS            string                            // default: runtime.GOOS
	GOARCH          string                            // default: runtime.GOARCH
	Probe           func(path string) (string, error) // default: probeVersion (runs `<path> --version`)
	Restart         func() error                      // default: triggerRestart (per-OS service restart)
	RestartDelay    time.Duration                     // default: restartDelay; delay before the detached restart fires
	RestartWatchdog time.Duration                     // default: restartWatchdog; how long a restart has to take effect
}

// Run executes the upgrade end to end, emitting Progress at each phase.
//
// Sequence (reusing #132's cordon): preflight → cordon + wait for jobs to
// drain to idle → download → verify checksum → stage + probe → swap →
// restart. On success the final emitted Progress is StateRestarting and Run
// returns nil BEFORE the service actually restarts (the restart is
// scheduled detached, after restartDelay). The caller then polls Status
// until the reported version matches the target.
//
// Any error before the swap emits StateFailed, re-uncordons the scheduler,
// and leaves the node running the old binary.
//
// The cordon is never allowed to outlive a failed upgrade. Every exit path —
// error return, panic, and the post-return case where the service manager
// simply never restarts us — un-cordons the scheduler, because a node that is
// drained and NOT upgraded is worse than one that never attempted the
// upgrade: it looks healthy while quietly accepting no work.
//
// Those paths all assume the upgrade eventually STOPS. The remaining way to
// hold a cordon forever is to hang, so the download carries a stall timeout
// and the whole post-drain phase carries an install budget; both turn a hang
// into an error, which the un-cordon above then handles like any other.
// A hard kill of the daemon needs no handling: `draining` lives only in the
// scheduler's memory, so a process that dies cordoned comes back up serving.
func Run(ctx context.Context, opts RunOptions, emit Emit) (retErr error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if emit == nil {
		emit = func(Progress) {}
	}
	goos := orString(opts.GOOS, runtime.GOOS)
	goarch := orString(opts.GOARCH, runtime.GOARCH)
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	drainTimeout := opts.DrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = defaultDrainTimeout
	}
	drainPoll := opts.DrainPoll
	if drainPoll <= 0 {
		drainPoll = defaultDrainPoll
	}

	target := NormalizeVersion(opts.TargetVersion)
	current := opts.CurrentVersion

	fail := func(format string, args ...any) error {
		err := fmt.Errorf(format, args...)
		emit(Progress{State: StateFailed, Message: err.Error(), CurrentVersion: current, TargetVersion: target})
		return err
	}

	// 1. Preflight.
	emit(Progress{State: StatePreflight, Message: "validating upgrade request", CurrentVersion: current, TargetVersion: target})
	if strings.TrimSpace(opts.TargetVersion) == "" {
		return fail("target_version is required (an explicit release tag; ephemerd never resolves \"latest\")")
	}
	if opts.BaseURLOverride == "" && !ValidVersion(target) {
		return fail("target_version %q is not a release tag of the form vX.Y.Z", opts.TargetVersion)
	}

	installPath := opts.InstallPath
	if installPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return fail("resolving own executable path: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		installPath = exe
	}

	if !opts.Force && SameVersion(current, target) {
		emit(Progress{State: StateUpToDate, Message: "already running " + target, CurrentVersion: current, TargetVersion: target})
		return nil
	}

	// 2. Cordon + drain to idle (unless skipped).
	//
	// From here on the node is NOT accepting jobs, and that is the state we
	// must never get stuck in. A node that is drained but not upgraded is
	// strictly worse than one that never tried: it looks healthy (the service
	// is Running, `ephemerd status` says ok) while silently taking no work.
	// So the reversal is a single idempotent closure, armed the instant we
	// cordon and fired from every way out of this function — an error return,
	// a panic, and (past the point where Run has already returned) the
	// restart supervisor's watchdog.
	didCordon := false
	var undrainOnce sync.Once
	undrain := func(why string) {
		if !didCordon || opts.Drainer == nil {
			return
		}
		undrainOnce.Do(func() {
			active := opts.Drainer.Uncordon()
			log.Warn("upgrade did not complete; scheduler UNCORDONED so this node keeps accepting jobs",
				"reason", why, "running_version", current, "target", target, "active_jobs", active)
		})
	}
	defer func() {
		// Cover panics too: an unexpected crash inside Run must not be the
		// one path that leaves the node cordoned.
		if r := recover(); r != nil {
			retErr = fmt.Errorf("upgrade panicked: %v", r)
			emit(Progress{State: StateFailed, Message: retErr.Error(), CurrentVersion: current, TargetVersion: target})
		}
		if retErr != nil {
			undrain(retErr.Error())
		}
	}()
	if !opts.NoDrain && opts.Drainer != nil {
		active := opts.Drainer.Cordon()
		didCordon = true
		emit(Progress{State: StateDraining, Message: "cordoned; waiting for running jobs to finish", ActiveJobs: active, CurrentVersion: current, TargetVersion: target})
		err := waitDrain(ctx, opts.Drainer, drainTimeout, drainPoll, func(a int) {
			emit(Progress{State: StateDraining, Message: fmt.Sprintf("waiting for %d job(s) to finish", a), ActiveJobs: a, CurrentVersion: current, TargetVersion: target})
		})
		if err != nil {
			return fail("draining before upgrade: %w", err)
		}
	}

	// 2b. Arm the cordon backstop. From here to the swap the node is drained
	// and doing work that has no business taking long; if it does, the
	// upgrade is wedged and the cordon is the damage. Cancelling this
	// context aborts the download/verify in flight, which surfaces as an
	// ordinary error and runs the un-cordon above.
	//
	// It is armed only now, after the drain: waiting on somebody's 40-minute
	// job is slow on purpose, and waitDrain already bounds that itself.
	installed := func() {} // disarms the backstop once the swap has landed
	if installTimeout := orDuration(opts.InstallTimeout, defaultInstallTimeout); installTimeout > 0 {
		var cancelInstall context.CancelFunc
		ctx, cancelInstall = context.WithCancel(ctx)
		defer cancelInstall()
		expired := new(atomic.Bool)
		watchdog := time.AfterFunc(installTimeout, func() {
			expired.Store(true)
			log.Error("upgrade: install phase exceeded its budget; aborting so the node does not stay cordoned",
				"budget", installTimeout, "target", target)
			cancelInstall()
		})
		installed = func() { watchdog.Stop() }
		defer watchdog.Stop()
		// Rewrite the cause on the way out: a bare "context canceled" from
		// deep inside io.Copy would otherwise be the only trace of this.
		defer func() {
			if retErr != nil && expired.Load() {
				retErr = fmt.Errorf("upgrade exceeded its %s install budget and was aborted (node stays on %s): %w",
					installTimeout, current, retErr)
			}
		}()
	}

	// 3. Prepare a staging dir on the SAME filesystem as the install path so
	// the swap is an atomic rename. Removed on exit; the .old backup lives
	// beside the install path and survives.
	stageDir := opts.StageDir
	if stageDir == "" {
		stageDir = filepath.Join(filepath.Dir(installPath), ".ephemerd-upgrade")
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return fail("creating staging dir %s: %w", stageDir, err)
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	// 4. Download the release archive.
	assetName := AssetName(target, goos, goarch)
	base := baseURL(target, opts.BaseURLOverride)
	archiveURL := base + "/" + assetName
	archivePath := filepath.Join(stageDir, assetName)
	stallTimeout := orDuration(opts.StallTimeout, defaultStallTimeout)
	emit(Progress{State: StateDownloading, Message: "downloading " + assetName, CurrentVersion: current, TargetVersion: target})
	if err := downloadFile(ctx, client, archiveURL, archivePath, stallTimeout, func(done, total int64) {
		emit(Progress{State: StateDownloading, Message: "downloading " + assetName, CurrentVersion: current, TargetVersion: target, BytesDownloaded: done, BytesTotal: total})
	}); err != nil {
		return fail("downloading %s: %w", archiveURL, err)
	}

	// 5. Verify the checksum BEFORE extracting or touching the live binary.
	// This is the supply-chain gate: a mismatch aborts with the live binary
	// untouched.
	emit(Progress{State: StateVerifying, Message: "verifying checksum", CurrentVersion: current, TargetVersion: target})
	sumsPath := filepath.Join(stageDir, checksumsName)
	if err := downloadFile(ctx, client, base+"/"+checksumsName, sumsPath, stallTimeout, nil); err != nil {
		return fail("downloading %s: %w", checksumsName, err)
	}
	sumsData, err := os.ReadFile(sumsPath)
	if err != nil {
		return fail("reading %s: %w", checksumsName, err)
	}
	sums, err := ParseChecksums(bytes.NewReader(sumsData))
	if err != nil {
		return fail("parsing %s: %w", checksumsName, err)
	}
	want, ok := sums[assetName]
	if !ok {
		return fail("%s has no entry for %s", checksumsName, assetName)
	}
	if err := verifyChecksum(archivePath, want); err != nil {
		return fail("%w — refusing to install; node stays on %s", err, current)
	}

	// 6. Stage: extract the binary and, when it can run on this host, probe
	// its --version to confirm it executes and reports the target BEFORE the
	// swap.
	emit(Progress{State: StateStaging, Message: "extracting new binary", CurrentVersion: current, TargetVersion: target})
	stagedBin := filepath.Join(stageDir, binaryEntryName(goos))
	if err := extractBinary(archivePath, goos, stagedBin); err != nil {
		return fail("extracting binary from %s: %w", assetName, err)
	}
	if goos == runtime.GOOS && goarch == runtime.GOARCH {
		probe := opts.Probe
		if probe == nil {
			probe = probeVersion
		}
		got, err := probe(stagedBin)
		if err != nil {
			return fail("probing staged binary: %w", err)
		}
		if !SameVersion(got, target) {
			return fail("staged binary reports version %q, expected %q — refusing swap", got, target)
		}
	}

	// 7. Swap. Past this line the on-disk binary has changed; the previous
	// one is retained as <name>.old for rollback.
	emit(Progress{State: StateSwapping, Message: "installing new binary", CurrentVersion: current, TargetVersion: target})
	backup, err := swapBinary(installPath, stagedBin)
	if err != nil {
		return fail("swapping binary at %s: %w", installPath, err)
	}
	// The cordon is now the restart supervisor's problem, not the install
	// backstop's; from here the node is SUPPOSED to be down briefly.
	installed()
	log.Info("upgrade: binary swapped", "install", installPath, "backup", backup, "from", current, "to", target)

	// 8. Restart into the new binary. Emit RESTARTING first (this is the last
	// message the caller sees over the stream), then schedule the detached
	// restart after a short delay and return nil. The caller treats a clean
	// stream end after RESTARTING as success-pending and polls Status for the
	// new version.
	emit(Progress{State: StateRestarting, Message: fmt.Sprintf("installed %s (kept %s for rollback); restarting service", target, filepath.Base(backup)), CurrentVersion: current, TargetVersion: target})
	restart := opts.Restart
	if restart == nil {
		// The helper is spawned from the backup — the image this very process
		// is running — rather than the newly installed binary. See
		// triggerRestart (Windows) for why; the Unix paths ignore both.
		restart = func() error { return triggerRestart(backup, installPath) }
	}
	delay := opts.RestartDelay
	if delay <= 0 {
		delay = restartDelay
	}
	watchdog := opts.RestartWatchdog
	if watchdog <= 0 {
		watchdog = restartWatchdog
	}

	// Supervise the restart instead of firing and forgetting it. Handing a
	// request to the service manager is not the same as being restarted:
	// v0.1.8 proved that a hand-off can succeed and still not take effect for
	// minutes, long past the point where the client has given up. If it does
	// not take, put the node back into service.
	go superviseRestart(restartSupervisor{
		restart:  restart,
		delay:    delay,
		watchdog: watchdog,
		shutdown: opts.Shutdown,
		log:      log,
		onFailed: func(cause error) {
			undrain(fmt.Sprintf("service restart into %s never happened: %v", target, cause))
			log.Error("upgrade INCOMPLETE: the new binary is installed but the service did not restart",
				"installed", target, "still_running", current, "install_path", installPath,
				"rollback_binary", backup, "remediation", manualRestartHint, "error", cause)
		},
	})
	return nil
}

// restartSupervisor is the parameter block for superviseRestart, kept as a
// struct so the (many) knobs stay named at the call site and the whole thing
// is trivially table-testable.
type restartSupervisor struct {
	restart  func() error
	delay    time.Duration // wait before the first attempt (lets the stream flush)
	watchdog time.Duration // how long an accepted request has to take effect
	shutdown <-chan struct{}
	log      *slog.Logger
	onFailed func(error) // called once, only when every attempt failed to take
}

// superviseRestart asks the service manager to restart us and then verifies
// that it happened.
//
// The verification is indirect but exact: a restart that takes effect stops
// this process, so if we are still executing after the watchdog, it did not.
// The one false positive would be a stop that is underway but slow, which is
// why s.shutdown short-circuits the wait — when the daemon starts going down
// the restart has demonstrably taken and we exit silently.
//
// Retrying is safe for the same reason (a successful attempt would have
// killed us), so a failed request gets one more, quicker try before we give
// up and hand control to onFailed.
func superviseRestart(s restartSupervisor) {
	if s.log == nil {
		s.log = slog.Default()
	}
	if !waitUnlessShutdown(s.delay, s.shutdown) {
		return
	}
	var lastErr error
	for attempt := 1; attempt <= restartAttempts; attempt++ {
		wait := s.watchdog
		if err := s.restart(); err != nil {
			lastErr = fmt.Errorf("attempt %d: requesting restart: %w", attempt, err)
			s.log.Error("upgrade: could not ask the service manager to restart", "attempt", attempt, "error", err)
			// Nothing was handed off, so there is nothing to wait for —
			// retry sooner. Never longer than the watchdog itself, which
			// also keeps this responsive under a test-sized watchdog.
			wait = restartRetryDelay
			if s.watchdog < wait {
				wait = s.watchdog
			}
		} else {
			lastErr = fmt.Errorf("attempt %d: restart was requested but this process was still running %s later", attempt, s.watchdog)
			s.log.Info("upgrade: restart requested; waiting for the service manager to take us down",
				"attempt", attempt, "watchdog", s.watchdog)
		}
		if !waitUnlessShutdown(wait, s.shutdown) {
			return // the daemon is going down: the restart took effect
		}
	}
	if s.onFailed != nil {
		s.onFailed(lastErr)
	}
}

// waitUnlessShutdown sleeps for d and reports true, or returns false as soon
// as shutdown fires. A nil shutdown channel simply never fires.
func waitUnlessShutdown(d time.Duration, shutdown <-chan struct{}) bool {
	if d <= 0 {
		select {
		case <-shutdown:
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-shutdown:
		return false
	case <-t.C:
		return true
	}
}

// waitDrain blocks until the scheduler reports zero active jobs or timeout.
// Mirrors the poll loop in `ephemerd drain --wait` (cmd/ephemerd/commands.go).
func waitDrain(ctx context.Context, d Drainer, timeout, poll time.Duration, onTick func(active int)) error {
	if d.ActiveJobs() == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			a := d.ActiveJobs()
			if onTick != nil {
				onTick(a)
			}
			if a == 0 {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("drain timed out after %s with %d job(s) still running", timeout, a)
			}
		}
	}
}

// NormalizeVersion trims and ensures a leading "v" so "0.1.7" and "v0.1.7"
// compare equal.
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// ValidVersion reports whether v is a release tag (vX.Y.Z[-suffix]).
func ValidVersion(v string) bool {
	return versionRe.MatchString(NormalizeVersion(v))
}

// SameVersion reports whether two version strings denote the same release.
// A blank or "dev" build never equals a real target, so an unstamped daemon
// is always eligible to upgrade.
func SameVersion(a, b string) bool {
	na, nb := NormalizeVersion(a), NormalizeVersion(b)
	if na == "" || nb == "" {
		return false
	}
	return na == nb
}

// assetExt is the archive extension per OS: zip on Windows, tar.gz elsewhere.
func assetExt(goos string) string {
	if goos == "windows" {
		return "zip"
	}
	return "tar.gz"
}

// binaryEntryName is the binary's name inside the archive and on disk.
func binaryEntryName(goos string) string {
	if goos == "windows" {
		return "ephemerd.exe"
	}
	return "ephemerd"
}

// AssetName derives the release asset filename for a target/OS/arch, e.g.
// ephemerd_v0.1.7_windows_amd64.zip. Confirmed against the v0.1.6 release.
func AssetName(version, goos, goarch string) string {
	return fmt.Sprintf("ephemerd_%s_%s_%s.%s", NormalizeVersion(version), goos, goarch, assetExt(goos))
}

// baseURL is the directory URL holding both the asset and checksums.txt.
func baseURL(version, override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	return defaultRepoBase + "/" + NormalizeVersion(version)
}

// ParseChecksums parses a `sha256sum`-style checksums.txt into a
// filename→hex-digest map. Malformed lines are skipped; an empty result is
// an error.
func ParseChecksums(r io.Reader) (map[string]string, error) {
	sums := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// sha256sum binary mode prefixes the name with '*'; tolerate it.
		name := strings.TrimPrefix(fields[1], "*")
		sums[name] = strings.ToLower(fields[0])
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("no checksum entries found")
	}
	return sums, nil
}

// verifyChecksum computes the sha256 of path and compares it to want (hex).
func verifyChecksum(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", filepath.Base(path), got, want)
	}
	return nil
}

// countingReader wraps an io.Reader, reporting cumulative bytes read through
// onProgress at most every ~250ms (plus a final call at EOF), and kicking a
// stall watchdog on every read that actually moved bytes.
//
// The watchdog is what makes a wedged transfer fail instead of hang. It is
// reset per read rather than per progress tick because the progress callback
// is rate-limited and a 250ms floor would make "no progress" and "no callback"
// two different things.
type countingReader struct {
	r          io.Reader
	total      int64
	done       int64
	onProgress func(done, total int64)
	last       time.Time
	alive      func() // reset the stall watchdog; nil when disabled
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.done += int64(n)
	if n > 0 && c.alive != nil {
		c.alive()
	}
	if c.onProgress != nil {
		now := time.Now()
		if err != nil || now.Sub(c.last) >= 250*time.Millisecond {
			c.last = now
			c.onProgress(c.done, c.total)
		}
	}
	return n, err
}

// downloadFile GETs url into dest, fsyncing before returning. onProgress may
// be nil. The archive is bounded (a release binary), so it streams straight
// to disk.
//
// stallTimeout aborts the transfer when no bytes arrive for that long; <= 0
// disables the check and restores the old wait-forever behavior. It covers
// the response headers too, so a connection that is accepted and then goes
// quiet fails at the same bound as one that dies mid-body.
func downloadFile(ctx context.Context, client *http.Client, url, dest string, stallTimeout time.Duration, onProgress func(done, total int64)) error {
	stalled := new(atomic.Bool)
	var alive func()
	if stallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		watchdog := time.AfterFunc(stallTimeout, func() {
			stalled.Store(true)
			cancel()
		})
		defer watchdog.Stop()
		alive = func() { watchdog.Reset(stallTimeout) }
	}
	// Translate the cancellation back into something an operator can act on;
	// "context canceled" alone reads like the caller went away.
	stallErr := func(err error) error {
		if err != nil && stalled.Load() {
			return fmt.Errorf("transfer stalled: no data for %s: %w", stallTimeout, err)
		}
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return stallErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if alive != nil {
		alive()
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	src := io.Reader(&countingReader{r: resp.Body, total: resp.ContentLength, onProgress: onProgress, alive: alive})
	if _, err := io.Copy(f, src); err != nil { //nolint:gosec // release asset, bounded size
		_ = f.Close()
		return stallErr(err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// orDuration resolves a zero-means-default, negative-means-disabled knob.
func orDuration(v, def time.Duration) time.Duration {
	if v == 0 {
		return def
	}
	return v
}

// probeVersion runs `<path> --version` and extracts the reported version.
func probeVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseVersionOutput(string(out)), nil
}

// parseVersionOutput pulls a version token out of `ephemerd --version`
// output ("ephemerd version v0.1.7"). Falls back to the last whitespace
// token so a format tweak doesn't break the probe outright.
func parseVersionOutput(out string) string {
	fields := strings.Fields(out)
	for _, f := range fields {
		if ValidVersion(f) {
			return NormalizeVersion(f)
		}
	}
	if len(fields) > 0 {
		return strings.TrimSpace(fields[len(fields)-1])
	}
	return strings.TrimSpace(out)
}

func orString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
