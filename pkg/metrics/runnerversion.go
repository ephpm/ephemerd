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
