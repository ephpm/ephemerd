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
// EVERYTHING here is off by default. With enabled = false nothing in
// pkg/runnerversion runs and the daemon behaves exactly as before. With
// enabled = true the daemon verifies its runner against a signed manifest
// at boot and watches for runners that exit without a job; it still never
// fetches a runner unless auto_upgrade = true as well.
type SelfHealToml struct {
	// Enabled is the master switch. Default false.
	Enabled bool `toml:"enabled"`

	// ManifestURL is the https URL of runner-manifest.json. The detached
	// signature is fetched from ManifestURL + ".minisig". Required when
	// Enabled.
	ManifestURL string `toml:"manifest_url"`

	// PublicKey overrides the minisign public key compiled into ephemerd
	// (one base64 line, as in a .pub file). Rotation and testing only.
	PublicKey string `toml:"public_key"`

	// Channel is "stable" (default) or "canary". Manifests for the other
	// channel are ignored.
	Channel string `toml:"channel"`

	// PollInterval is how often the manifest is re-checked. Default "1h".
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

	// AutoUpgrade is the break-glass switch: on a tripped breaker or a
	// below-floor boot, the HOST daemon fetches the manifest's runner,
	// verifies it against both the signed manifest and GitHub's
	// runners/downloads checksum, and swaps it in. Default false.
	// Requires Enabled.
	AutoUpgrade bool `toml:"auto_upgrade"`

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
	DefaultSelfHealPollInterval      = time.Hour
	DefaultSelfHealZeroWorkWindow    = 120 * time.Second
	DefaultSelfHealZeroWorkThreshold = 3
	DefaultSelfHealBreakerCooldown   = 15 * time.Minute
	DefaultSelfHealMaxArchiveMB      = 512
	DefaultSelfHealMinFreeGB         = 5
	DefaultSelfHealPRDeadline        = 72 * time.Hour
)

// Validate checks the table. It is cheap to call when Enabled is false
// (everything is accepted then, so a stray key cannot break a node that
// has not opted in).
func (c SelfHealToml) Validate() error {
	if !c.Enabled {
		if c.AutoUpgrade {
			return errors.New("[runner.self_heal] auto_upgrade = true requires enabled = true")
		}
		return nil
	}
	if !strings.HasPrefix(c.ManifestURL, "https://") {
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
