package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SelfHealToml configures [runner.self_heal]: the self-healing runner
// version subsystem (docs/arch/self-healing-runner.md).
//
// The subsystem is ON BY DEFAULT and is part of the product, not a feature
// flag: a node with no [runner.self_heal] table at all verifies its runner
// against the signed manifest at boot, watches for runners that exit
// without a job, masks a stale runner baked into the official Linux image,
// and — when nothing on the node is new enough — fetches the current runner
// from two independently verified sources. The one operator control that
// matters is the kill-switch:
//
//	[runner.self_heal]
//	enabled = false
//
// which turns ALL of it off without a redeploy (the zero-work breaker
// suspends provisioning, so a false positive must be recoverable by
// config). Everything else has a built-in default and is tuning.
//
// Every boolean here is a *bool so that "not set" can be told apart from
// "explicitly false": TOML decodes a missing key as the zero value, and
// the zero value of a plain bool would make the product default off.
type SelfHealToml struct {
	// Enabled is the master kill-switch. Default TRUE. Set enabled = false
	// to disable the whole subsystem; the daemon then behaves exactly as
	// it did before the subsystem existed.
	Enabled *bool `toml:"enabled"`

	// ManifestURL is the https URL of runner-manifest.json. The detached
	// signature is fetched from ManifestURL + ".minisig". Default:
	// DefaultSelfHealManifestURL, the stable release asset published by
	// the runner-manifest workflow on ephpm/ephemerd.
	ManifestURL string `toml:"manifest_url"`

	// PublicKey overrides the minisign public key compiled into ephemerd
	// (one base64 line, as in a .pub file). Rotation and testing only.
	PublicKey string `toml:"public_key"`

	// Channel is "stable" (default) or "canary". Manifests for the other
	// channel are ignored.
	Channel string `toml:"channel"`

	// PollInterval is how often the manifest is re-checked when nothing is
	// failing. Default "6h". The poll is a BACKSTOP, not the trigger: the
	// zero-work breaker kicks an immediate re-check the moment runners
	// start dying, so the poll's only jobs are to propagate a known-good
	// pin to nodes that have not failed yet and to catch a dropped kick.
	PollInterval string `toml:"poll_interval"`

	// ZeroWorkWindow: a runner that exits on its own sooner than this
	// after registering, with no job-started signal, is zero-work.
	// Default "120s".
	ZeroWorkWindow string `toml:"zero_work_window"`

	// ZeroWorkThreshold is the consecutive zero-work count per platform
	// that opens the breaker. Default 3.
	ZeroWorkThreshold int `toml:"zero_work_threshold"`

	// BreakerCooldown is how long provisioning stays suspended before one
	// probe runner is allowed. Default "15m".
	BreakerCooldown string `toml:"breaker_cooldown"`

	// AutoUpgrade is the break-glass network fetch: on a tripped breaker
	// or a below-floor runner, the HOST daemon fetches the manifest's
	// runner, verifies it against both the signed manifest and GitHub's
	// runners/downloads checksum, and uses it on the next job. Default
	// TRUE. Meaningful only when Enabled; setting it true with
	// enabled = false is a config error.
	AutoUpgrade *bool `toml:"auto_upgrade"`

	// ImageOverride lets the HOST mask a stale runner baked into an
	// official Linux runner image (runner-ci-linux is FROM
	// ghcr.io/actions/actions-runner and runs its own /home/runner) by
	// mounting a verified, newer runner tree over /home/runner for every
	// job until a rebuilt image rolls out. The tree comes from the embedded
	// runner when that is new enough, otherwise from the break-glass fetch
	// (which additionally needs auto_upgrade). Default TRUE. Meaningful
	// only when Enabled.
	ImageOverride *bool `toml:"image_override"`

	// MaxArchiveMB refuses runner downloads larger than this. Default 512.
	MaxArchiveMB int `toml:"max_archive_mb"`

	// MinFreeGB refuses a fetch when the cache volume has less free.
	// Default 5.
	MinFreeGB int `toml:"min_free_gb"`

	// AutoUpgradePRRepo is "owner/repo" the auto-PR that makes the new pin
	// permanent is opened against. Empty disables the auto-PR (the alert
	// still fires). Designed, not implemented yet.
	AutoUpgradePRRepo string `toml:"auto_upgrade_pr_repo"`

	// AutoUpgradePRDeadline: past this with the PR unmerged, nodes still
	// on a verified-cache runner escalate the alert. Default "72h".
	AutoUpgradePRDeadline string `toml:"auto_upgrade_pr_deadline"`
}

const (
	// DefaultSelfHealManifestURL is where the runner-manifest workflow on
	// ephpm/ephemerd publishes the signed manifest: a stable, overwritten
	// release asset (tag "runner-manifest"), so the URL never changes and
	// no node config needs to name it.
	DefaultSelfHealManifestURL = "https://github.com/ephpm/ephemerd/releases/download/runner-manifest/runner-manifest.json"

	// DefaultSelfHealPollInterval is the backstop cadence; failure is the
	// primary trigger (Detector.OnDegraded -> Controller.Kick).
	DefaultSelfHealPollInterval      = 6 * time.Hour
	DefaultSelfHealZeroWorkWindow    = 120 * time.Second
	DefaultSelfHealZeroWorkThreshold = 3
	DefaultSelfHealBreakerCooldown   = 15 * time.Minute
	DefaultSelfHealMaxArchiveMB      = 512
	DefaultSelfHealMinFreeGB         = 5
	DefaultSelfHealPRDeadline        = 72 * time.Hour
)

// ResolvedEnabled is the kill-switch: true unless the operator wrote
// enabled = false.
func (c SelfHealToml) ResolvedEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// ResolvedAutoUpgrade is true when the subsystem is enabled and the
// operator did not write auto_upgrade = false.
func (c SelfHealToml) ResolvedAutoUpgrade() bool {
	return c.ResolvedEnabled() && (c.AutoUpgrade == nil || *c.AutoUpgrade)
}

// ResolvedImageOverride is true when the subsystem is enabled and the
// operator did not write image_override = false.
func (c SelfHealToml) ResolvedImageOverride() bool {
	return c.ResolvedEnabled() && (c.ImageOverride == nil || *c.ImageOverride)
}

// ResolvedManifestURL returns ManifestURL or the built-in default.
func (c SelfHealToml) ResolvedManifestURL() string {
	if c.ManifestURL == "" {
		return DefaultSelfHealManifestURL
	}
	return c.ManifestURL
}

// Validate checks the table. It is called from Config.Load, so a bad
// table fails the daemon fast with a clear error instead of being
// discovered at the first poll. With enabled = false everything but the
// "requires enabled" invariants is accepted, so a stray tuning key cannot
// keep a node that opted out from starting.
func (c SelfHealToml) Validate() error {
	if !c.ResolvedEnabled() {
		if c.AutoUpgrade != nil && *c.AutoUpgrade {
			return errors.New("[runner.self_heal] auto_upgrade = true requires enabled = true")
		}
		if c.ImageOverride != nil && *c.ImageOverride {
			return errors.New("[runner.self_heal] image_override = true requires enabled = true")
		}
		return nil
	}
	if !strings.HasPrefix(c.ResolvedManifestURL(), "https://") {
		return errors.New("[runner.self_heal] manifest_url must be an https:// URL")
	}
	switch c.Channel {
	case "", "stable", "canary":
	default:
		return fmt.Errorf("[runner.self_heal] channel %q must be \"stable\" or \"canary\"", c.Channel)
	}
	for name, v := range map[string]string{
		"poll_interval":            c.PollInterval,
		"zero_work_window":         c.ZeroWorkWindow,
		"breaker_cooldown":         c.BreakerCooldown,
		"auto_upgrade_pr_deadline": c.AutoUpgradePRDeadline,
	} {
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("[runner.self_heal] %s: %w", name, err)
		}
		if d <= 0 {
			return fmt.Errorf("[runner.self_heal] %s must be positive", name)
		}
	}
	if c.ZeroWorkThreshold < 0 {
		return errors.New("[runner.self_heal] zero_work_threshold must be >= 1 (0 = default)")
	}
	if c.MaxArchiveMB < 0 || c.MinFreeGB < 0 {
		return errors.New("[runner.self_heal] max_archive_mb and min_free_gb must be >= 0")
	}
	if c.AutoUpgradePRRepo != "" {
		parts := strings.Split(c.AutoUpgradePRRepo, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("[runner.self_heal] auto_upgrade_pr_repo %q must be owner/repo", c.AutoUpgradePRRepo)
		}
	}
	return nil
}

// ResolvedChannel returns Channel or "stable".
func (c SelfHealToml) ResolvedChannel() string {
	if c.Channel == "" {
		return "stable"
	}
	return c.Channel
}

func (c SelfHealToml) resolvedDuration(v string, def time.Duration) time.Duration {
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// ResolvedPollInterval returns PollInterval or the default.
func (c SelfHealToml) ResolvedPollInterval() time.Duration {
	return c.resolvedDuration(c.PollInterval, DefaultSelfHealPollInterval)
}

// ResolvedZeroWorkWindow returns ZeroWorkWindow or the default.
func (c SelfHealToml) ResolvedZeroWorkWindow() time.Duration {
	return c.resolvedDuration(c.ZeroWorkWindow, DefaultSelfHealZeroWorkWindow)
}

// ResolvedBreakerCooldown returns BreakerCooldown or the default.
func (c SelfHealToml) ResolvedBreakerCooldown() time.Duration {
	return c.resolvedDuration(c.BreakerCooldown, DefaultSelfHealBreakerCooldown)
}

// ResolvedPRDeadline returns AutoUpgradePRDeadline or the default.
func (c SelfHealToml) ResolvedPRDeadline() time.Duration {
	return c.resolvedDuration(c.AutoUpgradePRDeadline, DefaultSelfHealPRDeadline)
}

// ResolvedZeroWorkThreshold returns ZeroWorkThreshold or the default.
func (c SelfHealToml) ResolvedZeroWorkThreshold() int {
	if c.ZeroWorkThreshold > 0 {
		return c.ZeroWorkThreshold
	}
	return DefaultSelfHealZeroWorkThreshold
}

// ResolvedMaxArchiveBytes returns the size cap in bytes.
func (c SelfHealToml) ResolvedMaxArchiveBytes() int64 {
	mb := c.MaxArchiveMB
	if mb <= 0 {
		mb = DefaultSelfHealMaxArchiveMB
	}
	return int64(mb) << 20
}

// ResolvedMinFreeBytes returns the free-space floor in bytes.
func (c SelfHealToml) ResolvedMinFreeBytes() uint64 {
	gb := c.MinFreeGB
	if gb <= 0 {
		gb = DefaultSelfHealMinFreeGB
	}
	return uint64(gb) << 30
}
