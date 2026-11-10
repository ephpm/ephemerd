package config

import (
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestSelfHealDefaultsOff(t *testing.T) {
	var rc RunnerConfig
	if _, err := toml.Decode(`max_concurrent = 2`, &rc); err != nil {
		t.Fatal(err)
	}
	sh := rc.SelfHeal
	if sh.Enabled || sh.AutoUpgrade {
		t.Fatal("self_heal must default off")
	}
	if err := sh.Validate(); err != nil {
		t.Fatalf("disabled table must validate: %v", err)
	}
	if sh.ResolvedChannel() != "stable" || sh.ResolvedPollInterval() != time.Hour ||
		sh.ResolvedZeroWorkWindow() != 120*time.Second || sh.ResolvedZeroWorkThreshold() != 3 ||
		sh.ResolvedBreakerCooldown() != 15*time.Minute || sh.ResolvedMaxArchiveBytes() != 512<<20 ||
		sh.ResolvedMinFreeBytes() != 5<<30 || sh.ResolvedPRDeadline() != 72*time.Hour {
		t.Fatalf("defaults: %+v", sh)
	}
}

func TestSelfHealDecodeAndValidate(t *testing.T) {
	var rc RunnerConfig
	src := `
[self_heal]
enabled = true
manifest_url = "https://github.com/ephpm/ephemerd/releases/download/runner-manifest/runner-manifest.json"
channel = "canary"
poll_interval = "30m"
zero_work_window = "90s"
zero_work_threshold = 5
breaker_cooldown = "5m"
auto_upgrade = true
max_archive_mb = 300
min_free_gb = 2
auto_upgrade_pr_repo = "ephpm/ephemerd"
auto_upgrade_pr_deadline = "48h"
`
	if _, err := toml.Decode(src, &rc); err != nil {
		t.Fatal(err)
	}
	sh := rc.SelfHeal
	if err := sh.Validate(); err != nil {
		t.Fatal(err)
	}
	if !sh.Enabled || !sh.AutoUpgrade || sh.ResolvedChannel() != "canary" || sh.ResolvedPollInterval() != 30*time.Minute ||
		sh.ResolvedZeroWorkWindow() != 90*time.Second || sh.ResolvedZeroWorkThreshold() != 5 ||
		sh.ResolvedBreakerCooldown() != 5*time.Minute || sh.ResolvedMaxArchiveBytes() != 300<<20 ||
		sh.ResolvedMinFreeBytes() != 2<<30 || sh.ResolvedPRDeadline() != 48*time.Hour {
		t.Fatalf("decoded: %+v", sh)
	}
}

func TestSelfHealValidateRejects(t *testing.T) {
	base := SelfHealToml{Enabled: true, ManifestURL: "https://example.com/m.json"}
	cases := map[string]struct {
		mutate func(*SelfHealToml)
		want   string
	}{
		"auto_upgrade without enabled": {func(c *SelfHealToml) { c.Enabled = false; c.AutoUpgrade = true }, "requires enabled"},
		"http manifest":                {func(c *SelfHealToml) { c.ManifestURL = "http://example.com/m.json" }, "https"},
		"missing manifest":             {func(c *SelfHealToml) { c.ManifestURL = "" }, "https"},
		"bad channel":                  {func(c *SelfHealToml) { c.Channel = "nightly" }, "channel"},
		"bad duration":                 {func(c *SelfHealToml) { c.ZeroWorkWindow = "soon" }, "zero_work_window"},
		"negative duration":            {func(c *SelfHealToml) { c.BreakerCooldown = "-1m" }, "positive"},
		"negative threshold":           {func(c *SelfHealToml) { c.ZeroWorkThreshold = -1 }, "zero_work_threshold"},
		"bad pr repo":                  {func(c *SelfHealToml) { c.AutoUpgradePRRepo = "ephemerd" }, "owner/repo"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}
