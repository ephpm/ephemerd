package config

import (
	"reflect"
	"testing"
	"time"
)

const testGiB = uint64(1024 * 1024 * 1024)

func TestImageGCDefaults(t *testing.T) {
	var z ImageGCConfig

	if !z.ImageGCEnabled() {
		t.Error("ImageGCEnabled() defaults to false; image GC must be on by default")
	}
	if got := z.ImageGCCheckInterval(); got != 60*time.Second {
		t.Errorf("ImageGCCheckInterval() = %v, want 60s", got)
	}
	if got := z.ImageGCHighWatermarkPercent(); got != 85 {
		t.Errorf("ImageGCHighWatermarkPercent() = %v, want 85", got)
	}
	if got := z.ImageGCLowWatermarkPercent(); got != 70 {
		t.Errorf("ImageGCLowWatermarkPercent() = %v, want 70", got)
	}
	if got := z.ImageGCMinFreeBytes(); got != 20*testGiB {
		t.Errorf("ImageGCMinFreeBytes() = %d, want 20 GiB", got)
	}
	// The absolute target defaults to twice the floor, mirroring the
	// 85%/70% percentage gap so both arms behave consistently.
	if got := z.ImageGCTargetFreeBytes(); got != 40*testGiB {
		t.Errorf("ImageGCTargetFreeBytes() = %d, want 40 GiB (2x the floor)", got)
	}
	// Age is deliberately NOT the primary mechanism.
	if got := z.ImageGCMaxAge(); got != 0 {
		t.Errorf("ImageGCMaxAge() = %v, want 0 (age backstop off by default)", got)
	}
}

func TestImageGCOverridesAndGuards(t *testing.T) {
	off := false
	c := ImageGCConfig{
		Enabled:              &off,
		CheckInterval:        30 * time.Second,
		HighWatermarkPercent: 90,
		LowWatermarkPercent:  60,
		MinFreeGB:            10,
		TargetFreeGB:         15,
		MaxAge:               72 * time.Hour,
	}
	if c.ImageGCEnabled() {
		t.Error("explicit enabled = false was not honored")
	}
	if got := c.ImageGCCheckInterval(); got != 30*time.Second {
		t.Errorf("CheckInterval = %v, want 30s", got)
	}
	if got := c.ImageGCHighWatermarkPercent(); got != 90 {
		t.Errorf("high = %v, want 90", got)
	}
	if got := c.ImageGCLowWatermarkPercent(); got != 60 {
		t.Errorf("low = %v, want 60", got)
	}
	if got := c.ImageGCMinFreeBytes(); got != 10*testGiB {
		t.Errorf("min free = %d, want 10 GiB", got)
	}
	if got := c.ImageGCTargetFreeBytes(); got != 15*testGiB {
		t.Errorf("target free = %d, want 15 GiB", got)
	}
	if got := c.ImageGCMaxAge(); got != 72*time.Hour {
		t.Errorf("max age = %v, want 72h", got)
	}

	// Out-of-range and negative values fall back to defaults rather than
	// failing startup — a typo in a watermark must not stop a node from
	// collecting, and a negative age must not evict everything.
	bad := ImageGCConfig{HighWatermarkPercent: 150, LowWatermarkPercent: -5, MaxAge: -time.Hour, CheckInterval: -time.Second}
	if got := bad.ImageGCHighWatermarkPercent(); got != 85 {
		t.Errorf("out-of-range high = %v, want the 85 default", got)
	}
	if got := bad.ImageGCLowWatermarkPercent(); got != 70 {
		t.Errorf("negative low = %v, want the 70 default", got)
	}
	if got := bad.ImageGCMaxAge(); got != 0 {
		t.Errorf("negative max age = %v, want 0", got)
	}
	if got := bad.ImageGCCheckInterval(); got != 0 {
		t.Errorf("negative interval = %v, want 0 (periodic sweep off)", got)
	}
}

// TestDindCacheMaxAgeIsNowOptIn pins the documented behavior change: the
// dind age backstop used to default to 7 days and was the only image
// eviction ephemerd had. Disk pressure is the trigger now, so an unset
// cache_max_age must mean "disabled", while an explicit value still works.
func TestDindCacheMaxAgeIsNowOptIn(t *testing.T) {
	var unset DindConfig
	if got := unset.DindCacheMaxAge(); got != 0 {
		t.Errorf("unset cache_max_age = %v, want 0 (disabled)", got)
	}
	set := DindConfig{CacheMaxAge: 48 * time.Hour}
	if got := set.DindCacheMaxAge(); got != 48*time.Hour {
		t.Errorf("explicit cache_max_age = %v, want 48h", got)
	}
	negative := DindConfig{CacheMaxAge: -time.Hour}
	if got := negative.DindCacheMaxAge(); got != 0 {
		t.Errorf("negative cache_max_age = %v, want 0", got)
	}
	// The prune interval keeps its old default: the loop still runs to
	// reap empty cache namespaces even with the age backstop off.
	if got := unset.DindCachePruneInterval(); got != 24*time.Hour {
		t.Errorf("unset cache_prune_interval = %v, want 24h", got)
	}
}

func TestBuildKitGCDefaults(t *testing.T) {
	var z BuildKitConfig

	// The whole point: an ephemerd node must never again run BuildKit with
	// no GC policy.
	if !z.BuildKitGCEnabled() {
		t.Error("BuildKitGCEnabled() defaults to false; the build cache would grow without bound")
	}
	if got := z.BuildKitGCReservedBytes(); got != 5*int64(testGiB) {
		t.Errorf("reserved = %d, want 5 GiB", got)
	}
	if got := z.BuildKitGCMaxUsedBytes(); got != 25*int64(testGiB) {
		t.Errorf("max used = %d, want 25 GiB", got)
	}
	if got := z.BuildKitGCMinFreeBytes(); got != 20*int64(testGiB) {
		t.Errorf("min free = %d, want 20 GiB", got)
	}
	if got := z.BuildKitGCKeepDuration(); got != 7*24*time.Hour {
		t.Errorf("keep duration = %v, want 168h", got)
	}
	if got := z.BuildKitGCEphemeralKeepDuration(); got != 48*time.Hour {
		t.Errorf("ephemeral keep = %v, want 48h", got)
	}
	if got := z.BuildKitGCEphemeralMaxUsedBytes(); got != 2*int64(testGiB) {
		t.Errorf("ephemeral max used = %d, want 2 GiB", got)
	}

	// The BuildKit free-space guard and the image collector's absolute
	// floor must agree, or the two collectors disagree about when the node
	// is tight.
	var img ImageGCConfig
	if int64(img.ImageGCMinFreeBytes()) != z.BuildKitGCMinFreeBytes() {
		t.Errorf("buildkit min_free_gb (%d) and image_gc min_free_gb (%d) defaults disagree",
			z.BuildKitGCMinFreeBytes(), img.ImageGCMinFreeBytes())
	}
}

func TestBuildKitGCNegativeDurationsDisableTheAgeArm(t *testing.T) {
	c := BuildKitConfig{GCKeepDuration: -time.Hour, GCEphemeralKeepDuration: -time.Hour}
	if got := c.BuildKitGCKeepDuration(); got != 0 {
		t.Errorf("negative keep duration = %v, want 0", got)
	}
	if got := c.BuildKitGCEphemeralKeepDuration(); got != 0 {
		t.Errorf("negative ephemeral keep duration = %v, want 0", got)
	}
}

func TestPinnedRunnerImages(t *testing.T) {
	cfg := &Config{
		Runner: RunnerConfig{
			DefaultImage: "ghcr.io/actions/actions-runner:latest",
			Images: map[string]map[string]string{
				"ephemerd": {"linux": "ephpm/ephemerd:runner-ci-linux-amd64", "windows": "ephpm/ephemerd:runner-ci-windows"},
			},
		},
		GitHub: GitHubConfig{
			Owner:               "ephpm",
			DefaultImageLinux:   "ephpm/ephemerd:runner-ci-linux-amd64", // duplicate of the per-repo entry
			DefaultImageWindows: "mcr.microsoft.com/windows/servercore:ltsc2022",
		},
		Gitea: GiteaConfig{InstanceURL: "https://gitea.example.com", JobImage: "gitea/runner-images:ubuntu-24.04"},
	}

	got := cfg.PinnedRunnerImages()
	want := []string{
		"ephpm/ephemerd:runner-ci-linux-amd64",
		"ephpm/ephemerd:runner-ci-windows",
		"gitea/runner-images:ubuntu-24.04",
		"ghcr.io/actions/actions-runner:latest",
		"mcr.microsoft.com/windows/servercore:ltsc2022",
	}
	// The result is sorted; sort the expectation the same way rather than
	// hand-ordering it.
	sortStrings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PinnedRunnerImages() = %v, want %v", got, want)
	}

	// A map iterates in random order — the output must not.
	for i := 0; i < 5; i++ {
		if again := cfg.PinnedRunnerImages(); !reflect.DeepEqual(again, got) {
			t.Fatalf("PinnedRunnerImages() is not deterministic: %v then %v", got, again)
		}
	}

	// An empty config must not produce phantom protections — protecting ""
	// would be meaningless and protecting nothing is correct.
	if got := (&Config{}).PinnedRunnerImages(); len(got) != 0 {
		t.Errorf("empty config pinned images = %v, want none", got)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
