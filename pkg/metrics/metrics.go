// Package metrics provides Prometheus metrics for ephemerd.
//
// Containerd's metrics (containerd_*) are automatically registered via the
// builtins import and appear alongside ephemerd's metrics on the same endpoint.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// JobsTotal counts completed jobs by provider, repo, and status.
	JobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ephemerd_jobs_total",
		Help: "Total number of jobs processed.",
	}, []string{"provider", "repo", "status"})

	// JobsActive tracks currently running jobs.
	JobsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ephemerd_jobs_active",
		Help: "Number of currently running jobs.",
	})

	// JobsQueuedTotal counts jobs received from webhook or poll.
	JobsQueuedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ephemerd_jobs_queued_total",
		Help: "Total number of jobs received (queued events).",
	})

	// JobDuration tracks the full lifecycle duration of a job.
	JobDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ephemerd_job_duration_seconds",
		Help:    "Time from container creation to destruction.",
		Buckets: []float64{10, 30, 60, 120, 300, 600, 1800, 3600, 7200},
	}, []string{"provider", "repo"})

	// JobStartup tracks time from queued event to runner registered with GitHub.
	JobStartup = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ephemerd_job_startup_seconds",
		Help:    "Time from queued event to runner environment ready.",
		Buckets: []float64{1, 2, 5, 10, 20, 30, 60, 120},
	}, []string{"repo"})

	// JobQueueWait tracks time spent waiting for a concurrency slot.
	JobQueueWait = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "ephemerd_job_queue_wait_seconds",
		Help:    "Time spent waiting for a concurrency semaphore slot.",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60},
	})

	// GitHubAPIRequests counts GitHub API calls by endpoint and status.
	GitHubAPIRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ephemerd_github_api_requests_total",
		Help: "Total GitHub API requests.",
	}, []string{"endpoint", "status_code"})

	// GitHubAPIRateRemaining tracks the remaining GitHub API rate limit.
	GitHubAPIRateRemaining = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ephemerd_github_api_rate_remaining",
		Help: "Remaining GitHub API rate limit quota.",
	})

	// GitHubPollTotal counts polling cycles.
	GitHubPollTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ephemerd_github_poll_total",
		Help: "Total number of GitHub API poll cycles executed.",
	})

	// GitHubWebhookEventsTotal counts received webhook events by type.
	GitHubWebhookEventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ephemerd_github_webhook_events_total",
		Help: "Total webhook events received.",
	}, []string{"event_type"})

	// JITRegistrationErrors counts JIT runner registration failures.
	JITRegistrationErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ephemerd_github_jit_registration_errors_total",
		Help: "Total JIT runner registration failures.",
	}, []string{"repo", "reason"})

	// UptimeSeconds tracks daemon uptime.
	UptimeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ephemerd_uptime_seconds",
		Help: "Time since daemon started in seconds.",
	})

	// ConcurrentCapacity reports the configured max concurrent jobs.
	ConcurrentCapacity = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ephemerd_concurrent_capacity",
		Help: "Maximum number of concurrent jobs (max_concurrent setting).",
	})

	// Draining reports whether the daemon is in drain mode.
	Draining = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ephemerd_draining",
		Help: "Whether the daemon is draining (1) or accepting jobs (0).",
	})
)
