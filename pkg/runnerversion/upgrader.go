package runnerversion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
}

// Result describes what Run did.
type Result struct {
	Version string
	SHA256  string
	Dir     string
	// Swapped is true when a new runner became current.
	Swapped bool
	// Reason is a short human string for the log/alert.
	Reason string
}

var (
	// ErrDisabled is returned when auto_upgrade is off.
	ErrDisabled = errors.New("runner auto-upgrade disabled (runner.self_heal.auto_upgrade = false)")
	// ErrSourcesDisagree means the manifest and GitHub do not describe the
	// same artifact. Either the manifest is stale or someone is lying;
	// both stop the upgrade.
	ErrSourcesDisagree = errors.New("runner sources disagree")
	// ErrManifestReplayed means the manifest is not newer than the last applied one.
	ErrManifestReplayed = errors.New("runner manifest not newer than last applied")
	// ErrWrongChannel means the manifest is for another channel.
	ErrWrongChannel = errors.New("runner manifest is for a different channel")
	// ErrDowngradeRefused means the manifest is older than current without rollback=true.
	ErrDowngradeRefused = errors.New("runner manifest would downgrade without rollback=true")
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
	last, err := u.Store.LastApplied()
	if err != nil {
		return Result{}, fmt.Errorf("reading last applied manifest: %w", err)
	}
	if !last.IsZero() && !m.PublishedAt.After(last) {
		return Result{}, fmt.Errorf("%w: manifest %s, last applied %s",
			ErrManifestReplayed, m.PublishedAt.Format(time.RFC3339), last.Format(time.RFC3339))
	}

	want, err := ManifestSource{Manifest: m}.Resolve(ctx, u.Platform)
	if err != nil {
		return Result{}, err
	}
	ghc, err := u.GitHub.Resolve(ctx, u.Platform)
	if err != nil {
		return Result{}, err
	}
	if ghc.Version != want.Version || ghc.SHA256 != want.SHA256 {
		metrics.RunnerManifestErrors.WithLabelValues("disagree").Inc()
		return Result{}, fmt.Errorf("%w: manifest says %s/%s, GitHub says %s/%s (re-sign the manifest if GitHub moved on)",
			ErrSourcesDisagree, want.Version, short(want.SHA256), ghc.Version, short(ghc.SHA256))
	}

	// Never silently downgrade; a replayed old manifest is caught above,
	// and a deliberate rollback must say so.
	cur, hasCur, err := u.Store.Current()
	if err != nil {
		return Result{}, err
	}
	if hasCur {
		cmp := runner.CompareVersions(runner.ParseVersionString(want.Version), runner.ParseVersionString(cur.Version))
		switch {
		case cmp == 0 && cur.SHA256 == want.SHA256:
			return Result{Version: cur.Version, SHA256: cur.SHA256, Dir: cur.Dir, Reason: "already current"}, nil
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
	// Fixed, grep-able message. Alert routing treats it as non-silenceable
	// until ephemerd_runner_version_info reports source="embedded" again.
	log.Error("runner auto-upgraded: PIN IS STALE",
		"version", want.Version, "sha256", want.SHA256, "dir", dir,
		"embedded", runner.Version,
		"action", "merge a release that pins this version in mage/download/download.go")
	return Result{Version: want.Version, SHA256: want.SHA256, Dir: dir, Swapped: true, Reason: "two-source verified upgrade"}, nil
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
