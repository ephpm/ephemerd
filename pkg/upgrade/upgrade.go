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

	// Test/override seams.
	InstallPath  string                            // default: resolved os.Executable()
	StageDir     string                            // default: <installdir>/.ephemerd-upgrade
	HTTPClient   *http.Client                      // default: http.DefaultClient (no timeout; ctx-governed)
	GOOS         string                            // default: runtime.GOOS
	GOARCH       string                            // default: runtime.GOARCH
	Probe        func(path string) (string, error) // default: probeVersion (runs `<path> --version`)
	Restart      func() error                      // default: triggerRestart (per-OS service restart)
	RestartDelay time.Duration                     // default: restartDelay; delay before the detached restart fires
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

	// 2. Cordon + drain to idle (unless skipped). On any failure before the
	// restart we uncordon so the node resumes claiming on the old binary.
	didCordon := false
	defer func() {
		if retErr != nil && didCordon && opts.Drainer != nil {
			opts.Drainer.Uncordon()
			log.Info("upgrade aborted; scheduler uncordoned, node stays on old binary", "current", current)
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
	emit(Progress{State: StateDownloading, Message: "downloading " + assetName, CurrentVersion: current, TargetVersion: target})
	if err := downloadFile(ctx, client, archiveURL, archivePath, func(done, total int64) {
		emit(Progress{State: StateDownloading, Message: "downloading " + assetName, CurrentVersion: current, TargetVersion: target, BytesDownloaded: done, BytesTotal: total})
	}); err != nil {
		return fail("downloading %s: %w", archiveURL, err)
	}

	// 5. Verify the checksum BEFORE extracting or touching the live binary.
	// This is the supply-chain gate: a mismatch aborts with the live binary
	// untouched.
	emit(Progress{State: StateVerifying, Message: "verifying checksum", CurrentVersion: current, TargetVersion: target})
	sumsPath := filepath.Join(stageDir, checksumsName)
	if err := downloadFile(ctx, client, base+"/"+checksumsName, sumsPath, nil); err != nil {
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
	log.Info("upgrade: binary swapped", "install", installPath, "backup", backup, "from", current, "to", target)

	// 8. Restart into the new binary. Emit RESTARTING first (this is the last
	// message the caller sees over the stream), then schedule the detached
	// restart after a short delay and return nil. The caller treats a clean
	// stream end after RESTARTING as success-pending and polls Status for the
	// new version.
	emit(Progress{State: StateRestarting, Message: fmt.Sprintf("installed %s (kept %s for rollback); restarting service", target, filepath.Base(backup)), CurrentVersion: current, TargetVersion: target})
	restart := opts.Restart
	if restart == nil {
		restart = triggerRestart
	}
	delay := opts.RestartDelay
	if delay <= 0 {
		delay = restartDelay
	}
	scheduleRestart(restart, delay, log)
	return nil
}

// scheduleRestart fires restart() after delay, detached from Run's return so
// it survives this call. It intentionally does not block: the caller's
// stream must flush the RESTARTING message and disconnect before the service
// goes down.
func scheduleRestart(restart func() error, delay time.Duration, log *slog.Logger) {
	time.AfterFunc(delay, func() {
		if err := restart(); err != nil {
			log.Error("upgrade: failed to trigger service restart — new binary is staged; restart manually", "error", err)
		}
	})
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
// onProgress at most every ~250ms (plus a final call at EOF).
type countingReader struct {
	r          io.Reader
	total      int64
	done       int64
	onProgress func(done, total int64)
	last       time.Time
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.done += int64(n)
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
func downloadFile(ctx context.Context, client *http.Client, url, dest string, onProgress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	src := io.Reader(resp.Body)
	if onProgress != nil {
		src = &countingReader{r: resp.Body, total: resp.ContentLength, onProgress: onProgress}
	}
	if _, err := io.Copy(f, src); err != nil { //nolint:gosec // release asset, bounded size
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
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
