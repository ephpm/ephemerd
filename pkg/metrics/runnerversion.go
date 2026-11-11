package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics for the self-healing runner version subsystem
// (docs/arch/self-healing-runner.md).
//
// Known prerequisite: mayfly never renders [metrics] bind_addr into node
// configs, so the listener binds loopback and Prometheus scrapes nothing
// fleet-wide. Until that is fixed these are visible only on the node.

// Values for the source label of RunnerVersionInfo.
const (
	RunnerSourceEmbedded      = "embedded"
	RunnerSourceVerifiedCache = "verified-cache"
	RunnerSourceBelowFloor    = "embedded-below-floor"
	// RunnerSourceImageOverride means a verified tree is mounted over an
	// official image's /home/runner (the Linux self-heal).
	RunnerSourceImageOverride = "image-override"
	// RunnerSourceGitHubBreakGlass means the node serves a runner fetched
	// from GitHub's runners/downloads with GitHub's checksum as the ONLY
	// authority — no signed manifest vouched for it. Reached only through
	// the break-glass path (breaker open, roll-forward). Alert while any
	// series carries this label; it clears when a re-signed manifest
	// covers the version.
	RunnerSourceGitHubBreakGlass = "github-breakglass"
)

var (
	// RunnerVersionInfo is 1 for the runner this node serves. Exactly one
	// series is 1 at a time (SetRunnerVersion resets the vector).
	RunnerVersionInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ephemerd_runner_version_info",
		Help: "The actions/runner version this node serves and where it came from (1 = active).",
	}, []string{"version", "source"})

	// RunnerPinAgeSeconds is now - published_at of the embedded pin's
	// actions/runner release. GitHub's minimum version historically trails
	// the current release by a few minors; alert on age.
	RunnerPinAgeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ephemerd_runner_pin_age_seconds",
		Help: "Age of the embedded actions/runner pin (seconds since its upstream release).",
	})

	// RunnerZeroWorkTotal counts runners that registered and exited on
	// their own within the window with no job-started signal.
	RunnerZeroWorkTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ephemerd_runner_zero_work_total",
		Help: "Runners that registered and exited quickly without ever receiving a job.",
	}, []string{"platform"})

	// RunnerBreakerOpen is 1 while a platform's zero-work breaker is OPEN
	// or HALF-OPEN (provisioning suspended).
	RunnerBreakerOpen = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ephemerd_runner_breaker_open",
		Help: "1 while the zero-work circuit breaker has suspended provisioning for a platform.",
	}, []string{"platform"})

	// RunnerImageOverrideActive is 1 while a verified runner tree is
	// mounted over the official image's /home/runner because the image's
	// baked-in runner is stale. Alert while 1: the permanent fix (rebuild
	// the runner-ci-linux image) is still outstanding.
	RunnerImageOverrideActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ephemerd_runner_image_override_active",
		Help: "1 while a verified runner is mounted over the official image's baked-in runner.",
	})

	// RunnerImageVersionInfo is 1 for the runner version baked into the
	// official Linux image this node uses, as probed at boot.
	RunnerImageVersionInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ephemerd_runner_image_version_info",
		Help: "The actions/runner version baked into the official Linux runner image (1 = probed).",
	}, []string{"image", "version"})

	// RunnerBreakGlassActive is 1 while the runner this node serves (or
	// mounts over the image) came from the break-glass GitHub-latest fetch
	// and no signed manifest has covered it yet. Non-silenceable: the
	// permanent fix is a re-signed manifest (and an image rebuild).
	RunnerBreakGlassActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ephemerd_runner_breakglass_active",
		Help: "1 while the node runs a runner fetched from GitHub without a signed manifest (break-glass).",
	})

	// RunnerBreakGlassTotal counts break-glass fetches that swapped in a
	// runner.
	RunnerBreakGlassTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ephemerd_runner_breakglass_total",
		Help: "Break-glass fetches of GitHub's latest runner that became current.",
	})

	// RunnerManifestErrors counts manifest problems by reason
	// ("fetch", "signature", "schema", "disagree", "checksum").
	RunnerManifestErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ephemerd_runner_manifest_errors_total",
		Help: "Runner manifest load/verify/agreement failures by reason.",
	}, []string{"reason"})
)

// SetRunnerVersion publishes the active runner, clearing any previous series.
func SetRunnerVersion(version, source string) {
	RunnerVersionInfo.Reset()
	RunnerVersionInfo.WithLabelValues(version, source).Set(1)
}

// SetRunnerImageVersion publishes the probed image runner, clearing any
// previous series (a rebuilt image changes the version label).
func SetRunnerImageVersion(image, version string) {
	RunnerImageVersionInfo.Reset()
	if version == "" {
		return
	}
	RunnerImageVersionInfo.WithLabelValues(image, version).Set(1)
}

// SetRunnerPinPublishedAt sets RunnerPinAgeSeconds from the embedded pin's
// release time.
func SetRunnerPinPublishedAt(publishedAt, now time.Time) {
	if publishedAt.IsZero() {
		return
	}
	age := now.Sub(publishedAt).Seconds()
	if age < 0 {
		age = 0
	}
	RunnerPinAgeSeconds.Set(age)
}
