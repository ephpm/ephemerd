package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func boolp(b bool) *bool { return &b }

// Default-on: a [runner] table with no self_heal sub-table at all gets the
// full subsystem with every default resolved, and validates as-is.
func TestSelfHealDefaultsOn(t *testing.T) {
	var rc RunnerConfig
	if _, err := toml.Decode(`max_concurrent = 2`, &rc); err != nil {
		t.Fatal(err)
	}
	sh := rc.SelfHeal
	if !sh.ResolvedEnabled() || !sh.ResolvedAutoUpgrade() || !sh.ResolvedImageOverride() {
		t.Fatalf("self_heal must default ON: %+v", sh)
	}
	if err := sh.Validate(); err != nil {
		t.Fatalf("empty table must validate out of the box: %v", err)
	}
	if sh.ResolvedManifestURL() != DefaultSelfHealManifestURL {
		t.Fatalf("manifest url default: %q", sh.ResolvedManifestURL())
	}
	if sh.ResolvedChannel() != "stable" || sh.ResolvedPollInterval() != 6*time.Hour ||
		sh.ResolvedZeroWorkWindow() != 120*time.Second || sh.ResolvedZeroWorkThreshold() != 3 ||
		sh.ResolvedBreakerCooldown() != 15*time.Minute || sh.ResolvedMaxArchiveBytes() != 512<<20 ||
		sh.ResolvedMinFreeBytes() != 5<<30 || sh.ResolvedPRDeadline() != 72*time.Hour {
		t.Fatalf("defaults: %+v", sh)
	}
}

// An EMPTY [self_heal] table is the same as no table.
func TestSelfHealEmptyTableIsDefaultOn(t *testing.T) {
	var rc RunnerConfig
	if _, err := toml.Decode("[self_heal]\n", &rc); err != nil {
		t.Fatal(err)
	}
	if !rc.SelfHeal.ResolvedEnabled() || rc.SelfHeal.Validate() != nil {
		t.Fatalf("empty [self_heal] must be default-on and valid: %+v", rc.SelfHeal)
	}
}

// The kill-switch: enabled = false turns everything off, including the
// sub-switches that default on.
func TestSelfHealKillSwitch(t *testing.T) {
	var rc RunnerConfig
	if _, err := toml.Decode("[self_heal]\nenabled = false\n", &rc); err != nil {
		t.Fatal(err)
	}
	sh := rc.SelfHeal
	if sh.ResolvedEnabled() || sh.ResolvedAutoUpgrade() || sh.ResolvedImageOverride() {
		t.Fatalf("enabled = false must disable every mechanism: %+v", sh)
	}
	if err := sh.Validate(); err != nil {
		t.Fatalf("disabled table must validate: %v", err)
	}
	// Disabled accepts otherwise-invalid tuning so a stray key cannot keep
	// an opted-out node from starting.
	sh.ZeroWorkWindow = "soon"
	sh.ManifestURL = "http://insecure"
	if err := sh.Validate(); err != nil {
		t.Fatalf("disabled table must not validate tuning: %v", err)
	}
}

func TestSelfHealDecodeAndValidate(t *testing.T) {
	var rc RunnerConfig
	src := `
[self_heal]
enabled = true
manifest_url = "https://example.com/runner-manifest.json"
channel = "canary"
poll_interval = "30m"
zero_work_window = "90s"
zero_work_threshold = 5
breaker_cooldown = "5m"
auto_upgrade = false
image_override = false
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
	if !sh.ResolvedEnabled() || sh.ResolvedAutoUpgrade() || sh.ResolvedImageOverride() ||
		sh.ResolvedManifestURL() != "https://example.com/runner-manifest.json" ||
		sh.ResolvedChannel() != "canary" || sh.ResolvedPollInterval() != 30*time.Minute ||
		sh.ResolvedZeroWorkWindow() != 90*time.Second || sh.ResolvedZeroWorkThreshold() != 5 ||
		sh.ResolvedBreakerCooldown() != 5*time.Minute || sh.ResolvedMaxArchiveBytes() != 300<<20 ||
		sh.ResolvedMinFreeBytes() != 2<<30 || sh.ResolvedPRDeadline() != 48*time.Hour {
		t.Fatalf("decoded: %+v", sh)
	}
}

func TestSelfHealValidateRejects(t *testing.T) {
	base := SelfHealToml{}
	cases := map[string]struct {
		mutate func(*SelfHealToml)
		want   string
	}{
		"auto_upgrade without enabled":   {func(c *SelfHealToml) { c.Enabled = boolp(false); c.AutoUpgrade = boolp(true) }, "requires enabled"},
		"image_override without enabled": {func(c *SelfHealToml) { c.Enabled = boolp(false); c.ImageOverride = boolp(true) }, "requires enabled"},
		"http manifest":                  {func(c *SelfHealToml) { c.ManifestURL = "http://example.com/m.json" }, "https"},
		"bad channel":                    {func(c *SelfHealToml) { c.Channel = "nightly" }, "channel"},
		"bad duration":                   {func(c *SelfHealToml) { c.ZeroWorkWindow = "soon" }, "zero_work_window"},
		"negative duration":              {func(c *SelfHealToml) { c.BreakerCooldown = "-1m" }, "positive"},
		"negative threshold":             {func(c *SelfHealToml) { c.ZeroWorkThreshold = -1 }, "zero_work_threshold"},
		"bad pr repo":                    {func(c *SelfHealToml) { c.AutoUpgradePRRepo = "ephemerd" }, "owner/repo"},
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

// Config.Load fails fast on a bad [runner.self_heal] table, and succeeds
// with no table at all (default-on needs no keys).
func TestLoadValidatesSelfHeal(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// github.token keeps the provider validation happy.
	okCfg := "[github]\ntoken = \"t\"\nowner = \"o\"\n"
	if _, err := Load(write(okCfg)); err != nil {
		t.Fatalf("default-on config must load: %v", err)
	}
	cfg, err := Load(write(okCfg + "[runner.self_heal]\nenabled = false\n"))
	if err != nil {
		t.Fatalf("kill-switch config must load: %v", err)
	}
	if cfg.Runner.SelfHeal.ResolvedEnabled() {
		t.Fatal("kill-switch not honoured through Load")
	}
	_, err = Load(write(okCfg + "[runner.self_heal]\nzero_work_window = \"soon\"\n"))
	if err == nil || !strings.Contains(err.Error(), "zero_work_window") {
		t.Fatalf("Load must surface the self_heal validation error, got %v", err)
	}
	_, err = Load(write(okCfg + "[runner.self_heal]\nenabled = false\nauto_upgrade = true\n"))
	if err == nil || !strings.Contains(err.Error(), "requires enabled") {
		t.Fatalf("Load must reject auto_upgrade without enabled, got %v", err)
	}
}
