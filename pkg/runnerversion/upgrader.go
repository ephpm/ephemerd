package runnerversion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/runner"
)

// Upgrader is the break-glass path: resolve the current runner from TWO
// independent sources (the operator-signed manifest and GitHub's own
// runners/downloads statement), require them to agree on version and
// SHA-256, fetch, verify, extract, swap. It is gated on AutoUpgrade and
// returns ErrDisabled before touching the network when that is false.
//
// Host daemon only. Nothing here may run inside a job VM or container.
//
// ONE exception to the two-source rule exists, and it is the only path in
// ephemerd where GitHub's REST API is the sole authority for a binary the
// fleet runs as root: see breakGlass below. It never applies in steady
// state.
type Upgrader struct {
	// AutoUpgrade is [runner.self_heal] auto_upgrade. Default false.
	AutoUpgrade bool
	// Channel is the node's channel; manifests on another are ignored.
	Channel Channel
	// Platform is PlatformKey(GOOS, GOARCH) of the runner this node serves.
	Platform string

	Manifest ManifestLoader
	GitHub   Source
	Fetcher  *Fetcher
	Store    *Store
	Log      *slog.Logger
	Now      func() time.Time

	// BreakGlassOpen reports whether the break-glass window is open: the
	// zero-work breaker for the platform class this runner serves is OPEN,
	// i.e. the fleet is empirically dying on the runner it has. Nil = the
	// window never opens (no detector on this daemon).
	BreakGlassOpen func() bool
	// BreakGlassCooldown is the minimum time between two break-glass
	// attempts (one shot per breaker cooldown). <= 0 = DefaultBreakerCooldown.
	BreakGlassCooldown time.Duration

	bgMu           sync.Mutex
	lastBreakGlass time.Time
}

// Result describes what Run did.
type Result struct {
	Version string
	SHA256  string
	Dir     string
	// Swapped is true when a new runner became current.
	Swapped bool
	// BreakGlass is true when the runner that became current (or was
	// confirmed current) was fetched on GitHub's word alone.
	BreakGlass bool
	// Reason is a short human string for the log/alert.
	Reason string
}

var (
	// ErrDisabled is returned when auto_upgrade is off.
	ErrDisabled = errors.New("runner auto-upgrade disabled (runner.self_heal.auto_upgrade = false)")
	// ErrSourcesDisagree means the manifest and GitHub do not describe the
	// same artifact. Either the manifest is stale or someone is lying;
	// both stop the upgrade (unless the break-glass window is open and
	// GitHub is strictly ahead).
	ErrSourcesDisagree = errors.New("runner sources disagree")
	// ErrManifestReplayed means the manifest is not newer than the last applied one.
	ErrManifestReplayed = errors.New("runner manifest not newer than last applied")
	// ErrWrongChannel means the manifest is for another channel.
	ErrWrongChannel = errors.New("runner manifest is for a different channel")
	// ErrDowngradeRefused means the manifest is older than current without rollback=true.
	ErrDowngradeRefused = errors.New("runner manifest would downgrade without rollback=true")
	// ErrBreakGlassCooldown means a break-glass fetch was attempted within
	// the cooldown of the previous one.
	ErrBreakGlassCooldown = errors.New("runner break-glass fetch already attempted within the breaker cooldown")
)

// Run performs one upgrade attempt. Safe to call repeatedly: when the store
// is already on the manifest's version it returns Swapped=false.
func (u *Upgrader) Run(ctx context.Context) (Result, error) {
	if !u.AutoUpgrade {
		return Result{}, ErrDisabled
	}
	if u.Manifest == nil || u.GitHub == nil || u.Fetcher == nil || u.Store == nil {
		return Result{}, errors.New("upgrader is missing a dependency")
	}
	log := u.Log
	if log == nil {
		log = slog.Default()
	}
	now := u.Now
	if now == nil {
		now = time.Now
	}

	m, err := u.Manifest.Load(ctx)
	if err != nil {
		metrics.RunnerManifestErrors.WithLabelValues(manifestErrorReason(err)).Inc()
		return Result{}, err
	}
	if u.Channel != "" && m.Channel != u.Channel {
		return Result{}, fmt.Errorf("%w: manifest %s, node %s", ErrWrongChannel, m.Channel, u.Channel)
	}

	want, err := ManifestSource{Manifest: m}.Resolve(ctx, u.Platform)
	if err != nil {
		return Result{}, err
	}
	ghc, err := u.GitHub.Resolve(ctx, u.Platform)
	if err != nil {
		return Result{}, err
	}
	cur, hasCur, err := u.Store.Current()
	if err != nil {
		return Result{}, err
	}
	if ghc.Version != want.Version || ghc.SHA256 != want.SHA256 {
		metrics.RunnerManifestErrors.WithLabelValues("disagree").Inc()
		disagree := fmt.Errorf("%w: manifest says %s/%s, GitHub says %s/%s (re-sign the manifest if GitHub moved on)",
			ErrSourcesDisagree, want.Version, short(want.SHA256), ghc.Version, short(ghc.SHA256))
		if !u.breakGlassEligible(ghc, want, cur, hasCur, log) {
			return Result{}, disagree
		}
		return u.breakGlass(ctx, ghc, want, log, now)
	}

	// The two sources agree. The replay guard applies to APPLYING a
	// manifest, so it sits here, after the agreement check and not before:
	// a manifest that was applied once must not block the break-glass
	// evaluation above when GitHub later moves ahead of it.
	last, err := u.Store.LastApplied()
	if err != nil {
		return Result{}, fmt.Errorf("reading last applied manifest: %w", err)
	}
	if !last.IsZero() && !m.PublishedAt.After(last) {
		return Result{}, fmt.Errorf("%w: manifest %s, last applied %s",
			ErrManifestReplayed, m.PublishedAt.Format(time.RFC3339), last.Format(time.RFC3339))
	}

	// Never silently downgrade; a replayed old manifest is caught above,
	// and a deliberate rollback must say so.
	if hasCur {
		cmp := runner.CompareVersions(runner.ParseVersionString(want.Version), runner.ParseVersionString(cur.Version))
		switch {
		case cmp == 0 && cur.SHA256 == want.SHA256:
			return u.alreadyCurrent(m, cur, log)
		case cmp < 0 && !m.Rollback:
			return Result{}, fmt.Errorf("%w: manifest %s, current %s", ErrDowngradeRefused, want.Version, cur.Version)
		}
	}

	meta := Sidecar{
		Version:   want.Version,
		Platform:  u.Platform,
		SHA256:    want.SHA256,
		FetchedAt: now(),
		Sources:   []string{want.Source, ghc.Source},
	}
	if _, err := u.Fetcher.Fetch(ctx, want.URL, meta); err != nil {
		if errors.Is(err, ErrChecksumMismatch) {
			metrics.RunnerManifestErrors.WithLabelValues("checksum").Inc()
		}
		return Result{}, err
	}
	dir, err := u.Store.Extract(want.SHA256, want.Version)
	if err != nil {
		return Result{}, err
	}
	if err := u.Store.Swap(want.SHA256, want.Version); err != nil {
		return Result{}, err
	}
	if err := u.Store.SetLastApplied(m.PublishedAt); err != nil {
		log.Warn("recording applied manifest", "error", err)
	}
	if err := u.Store.Prune(); err != nil {
		log.Warn("pruning runner cache after swap", "error", err)
	}
	metrics.SetRunnerVersion(want.Version, metrics.RunnerSourceVerifiedCache)
	metrics.RunnerBreakGlassActive.Set(0)
	// Fixed, grep-able message. Alert routing treats it as non-silenceable
	// until ephemerd_runner_version_info reports source="embedded" again.
	log.Error("runner auto-upgraded: PIN IS STALE",
		"version", want.Version, "sha256", want.SHA256, "dir", dir,
		"embedded", runner.Version,
		"action", "merge a release that pins this version in mage/download/download.go")
	return Result{Version: want.Version, SHA256: want.SHA256, Dir: dir, Swapped: true, Reason: "two-source verified upgrade"}, nil
}

// alreadyCurrent handles "the manifest describes exactly what is current".
// If that blob came from a break-glass fetch (GitHub was its only
// voucher), the manifest has now caught up: record it as a second source
// so the selector stops labelling the runner break-glass, and clear the
// alert. Normal two-party verification is thereby resumed without a
// re-download — the bytes are the same bytes, by hash.
func (u *Upgrader) alreadyCurrent(m *Manifest, cur Pointer, log *slog.Logger) (Result, error) {
	res := Result{Version: cur.Version, SHA256: cur.SHA256, Dir: cur.Dir, Reason: "already current"}
	// A missing or unreadable sidecar is not a failure of "already
	// current" (Verify at boot handles broken blobs); it just means there
	// is no break-glass state to clear.
	sc, scErr := u.Store.Sidecar(cur.SHA256)
	if scErr != nil {
		log.Warn("reading sidecar of the current runner", "sha256", cur.SHA256, "error", scErr)
	}
	if scErr == nil && sc.IsBreakGlass() {
		if err := u.Store.AddSidecarSource(cur.SHA256, SourceManifest); err != nil {
			log.Warn("recording manifest as a source for the break-glass runner", "error", err)
		} else {
			if err := u.Store.SetLastApplied(m.PublishedAt); err != nil {
				log.Warn("recording applied manifest", "error", err)
			}
			metrics.SetRunnerVersion(cur.Version, metrics.RunnerSourceVerifiedCache)
			metrics.RunnerBreakGlassActive.Set(0)
			log.Warn("signed manifest caught up with the break-glass runner; two-source verification resumed",
				"version", cur.Version, "sha256", cur.SHA256)
			res.Reason = "manifest caught up with break-glass runner"
		}
	}
	return res, nil
}

// breakGlassEligible is the whole decision for the ONE path where GitHub's
// runners/downloads statement is the sole authority for a runner binary.
// Every condition is a hard gate:
//
//   - the window is open: the zero-work breaker for this runner's platform
//     class is OPEN — the fleet is demonstrably dying on what it has. This
//     never applies in steady state; sources disagreeing while the breaker
//     is closed stay a refusal and an alert;
//   - roll-forward only: GitHub's version is STRICTLY newer than the signed
//     manifest's. A GitHub that is equal (different hash) or behind our
//     signed pin is a spoof or a regression, not a heal signal;
//   - and strictly newer than the runner the node already has (current
//     cache or embedded): fetching what we already run cannot help;
//   - one shot per breaker cooldown, enforced in breakGlass.
//
// Size cap, free-space floor and the SHA pin (GitHub's sha256_checksum,
// verified over the whole stream before anything is extracted) all still
// apply in Fetcher/Store.
func (u *Upgrader) breakGlassEligible(ghc, want Candidate, cur Pointer, hasCur bool, log *slog.Logger) bool {
	if u.BreakGlassOpen == nil || !u.BreakGlassOpen() {
		return false
	}
	ghv := runner.ParseVersionString(ghc.Version)
	if runner.CompareVersions(ghv, runner.ParseVersionString(want.Version)) <= 0 {
		log.Error("runner sources disagree with the breaker open, but GitHub is not ahead of the signed manifest: refusing (spoof or regression, not a heal signal)",
			"manifest", want.Version, "github", ghc.Version)
		return false
	}
	have := runner.ParseVersionString(runner.Version)
	if hasCur {
		if cv := runner.ParseVersionString(cur.Version); runner.CompareVersions(cv, have) > 0 {
			have = cv
		}
	}
	if runner.CompareVersions(ghv, have) <= 0 {
		log.Warn("breaker open and GitHub is ahead of the manifest, but not ahead of the runner this node already has; nothing to fetch",
			"github", ghc.Version, "have", versionString(have))
		return false
	}
	return true
}

// breakGlass fetches GitHub's current runner on GitHub's word alone and
// swaps it in. See breakGlassEligible for the gates; this adds the
// one-shot-per-cooldown and the audit trail: the sidecar lists GitHub as
// the ONLY source, the version metric carries source="github-breakglass",
// and ephemerd_runner_breakglass_active stays 1 until a re-signed
// manifest covers the version (alreadyCurrent) or a newer two-source
// upgrade replaces it.
func (u *Upgrader) breakGlass(ctx context.Context, ghc, want Candidate, log *slog.Logger, now func() time.Time) (Result, error) {
	cooldown := u.BreakGlassCooldown
	if cooldown <= 0 {
		cooldown = DefaultBreakerCooldown
	}
	u.bgMu.Lock()
	if !u.lastBreakGlass.IsZero() && now().Sub(u.lastBreakGlass) < cooldown {
		u.bgMu.Unlock()
		return Result{}, fmt.Errorf("%w: last attempt %s ago, cooldown %s",
			ErrBreakGlassCooldown, now().Sub(u.lastBreakGlass).Round(time.Second), cooldown)
	}
	u.lastBreakGlass = now()
	u.bgMu.Unlock()

	log.Warn("runner break-glass: breaker open and GitHub is ahead of the signed manifest; fetching GitHub's current runner on GitHub's checksum ALONE",
		"manifest", want.Version, "github", ghc.Version, "sha256", ghc.SHA256, "url", ghc.URL)
	meta := Sidecar{
		Version:   ghc.Version,
		Platform:  u.Platform,
		SHA256:    ghc.SHA256,
		FetchedAt: now(),
		Sources:   []string{ghc.Source}, // GitHub only: the audit trail of this path
	}
	if _, err := u.Fetcher.Fetch(ctx, ghc.URL, meta); err != nil {
		if errors.Is(err, ErrChecksumMismatch) {
			metrics.RunnerManifestErrors.WithLabelValues("checksum").Inc()
		}
		return Result{}, fmt.Errorf("break-glass fetch: %w", err)
	}
	dir, err := u.Store.Extract(ghc.SHA256, ghc.Version)
	if err != nil {
		return Result{}, err
	}
	if err := u.Store.Swap(ghc.SHA256, ghc.Version); err != nil {
		return Result{}, err
	}
	// Deliberately NOT SetLastApplied: no manifest was applied. The
	// re-signed one that follows must not be treated as a replay.
	if err := u.Store.Prune(); err != nil {
		log.Warn("pruning runner cache after break-glass swap", "error", err)
	}
	metrics.SetRunnerVersion(ghc.Version, metrics.RunnerSourceGitHubBreakGlass)
	metrics.RunnerBreakGlassActive.Set(1)
	metrics.RunnerBreakGlassTotal.Inc()
	// Fixed, grep-able line; non-silenceable while
	// ephemerd_runner_breakglass_active is 1.
	log.Warn("self-heal fetched un-manifest-signed runner from GitHub to clear an open breaker; re-sign the manifest (and rebuild the image) to make this permanent",
		"version", ghc.Version, "sha256", ghc.SHA256, "dir", dir, "manifest", want.Version, "embedded", runner.Version,
		"action", "run the Runner manifest workflow (bump RunnerVersion/RunnerFloor in mage/download) so the signed manifest covers "+ghc.Version)
	return Result{Version: ghc.Version, SHA256: ghc.SHA256, Dir: dir, Swapped: true, BreakGlass: true, Reason: "break-glass fetch of GitHub's latest (breaker open, roll-forward)"}, nil
}

func versionString(parts []int) string {
	if len(parts) == 0 {
		return "unknown"
	}
	s := ""
	for i, p := range parts {
		if i > 0 {
			s += "."
		}
		s += fmt.Sprint(p)
	}
	return s
}

func manifestErrorReason(err error) string {
	switch {
	case errors.Is(err, ErrBadSignature), errors.Is(err, ErrKeyMismatch), errors.Is(err, ErrNoPublicKey):
		return "signature"
	case errors.Is(err, ErrManifestInvalid):
		return "schema"
	default:
		return "fetch"
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
