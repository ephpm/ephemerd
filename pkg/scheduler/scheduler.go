package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/artifacts"
	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/names"
	"github.com/ephpm/ephemerd/pkg/native"
	"github.com/ephpm/ephemerd/pkg/providers"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"github.com/ephpm/ephemerd/pkg/tunnel"
	"github.com/ephpm/ephemerd/pkg/vm"
	gh "github.com/google/go-github/v72/github"
)

// Config for the scheduler.
type Config struct {
	Runtime          *runtime.Runtime
	Providers        []providers.Provider
	Artifacts        *artifacts.Extractor // OCI image layer extractor for macOS VM jobs (nil if not available)
	LinuxDispatcher  *DispatchClient      // if non-nil, Linux jobs are dispatched to a Linux VM worker via gRPC
	MacOSVMConfig    *vm.MacOSVMConfig    // if non-nil, macOS-native jobs are enabled (darwin only)
	DataDir          string               // ephemerd data directory (used for artifact extraction paths)
	MaxConcurrent    int
	MaxMacOSVMs      int // max concurrent macOS VMs (Vz limit; default auto-detected)
	Labels           []string
	PollInterval     time.Duration   // if >0, use polling mode (default)
	WebhookPort      int             // listen port for health/webhook server
	WebhookSecret    string          // webhook signature secret
	TLSCert          string          // TLS certificate path
	TLSKey           string          // TLS private key path
	Tunnel           tunnel.Provider // if non-nil, creates a public tunnel for webhooks
	TunnelMaxRetries int             // max consecutive reconnect failures before fallback to polling (0 = default 5)
	JobTimeout       time.Duration
	ShutdownTimeout  time.Duration
	LogRetention     time.Duration // max age for job log files (default 7d)

	// Retry configures the claim/provision retry queue. When the initial
	// attempt to claim a queued job fails with a retryable error
	// (rate-limit exhausted, transient 5xx, network), the job is
	// enqueued and re-attempted on a backoff ladder rather than lost.
	// GitHub does not re-deliver workflow_job webhooks. Leave zero-valued
	// (Enabled=false) to keep the pre-existing "log and drop" behavior.
	Retry RetryConfig

	// OrphanSweep configures teardown of dispatched runners that were
	// never observed picking up a job. GitHub schedules JIT runners onto
	// ANY queued job with matching labels, so the runner dispatched "for"
	// a job may end up running a different one — leaving the runner that
	// was dispatched for THAT job idle with no job-completion event ever
	// pointing at it. The sweep destroys such runners once they have been
	// idle-unbound for Grace. Only active in webhook mode and only for
	// runners dispatched via providers that report runner assignments
	// (providers.RunnerNameReporter) — otherwise "never observed bound"
	// would just mean "we had no way to observe it".
	OrphanSweep OrphanSweepConfig

	// RunnerImageForRepo resolves the per-repo, per-OS image override
	// configured under [runner.images]. Returns "" when no override is
	// set; the scheduler then falls back to the provider per-OS default
	// and finally the runtime's host-aware default. Nil-safe.
	RunnerImageForRepo func(repo, os string) string

	MaxNativeMac     int                      // max concurrent native macOS jobs (default 4)
	MacOSModeForRepo func(repo string) string // returns "native" or "vm" per repo (nil = always VM)
	NativeMacUser    string                   // non-root user for native macOS runner processes
	RunnerDir        string                   // path to extracted GHA runner binary dir (runner.Manager.Dir())
	PrivateKeyPath   string                   // GitHub App private_key_path, denied read access in the native sandbox (empty for PAT auth)

	Log *slog.Logger
}

// resolveImage returns the runner image to launch for an event.
//
// Resolution order:
//
//  1. Image declared in the workflow YAML (FetchJobImage)
//  2. Per-repo override from [runner.images.<repo>].<os>
//  3. Provider per-OS default (DefaultImageFor)
//  4. Empty — runtime.Create picks its host-aware fallback
func (s *Scheduler) resolveImage(ctx context.Context, event *providers.JobEvent, os string) string {
	if event == nil || event.Provider == nil {
		return ""
	}
	if img := event.Provider.FetchJobImage(ctx, event); img != "" {
		return img
	}
	if s.cfg.RunnerImageForRepo != nil {
		if img := s.cfg.RunnerImageForRepo(event.Repo, os); img != "" {
			return img
		}
	}
	return event.Provider.DefaultImageFor(os)
}

// OrphanSweepConfig tunes the orphaned-runner sweep. Zero-valued =
// disabled (matching pre-existing behavior); the CLI enables it by
// default with a 10-minute grace window.
type OrphanSweepConfig struct {
	// Enabled toggles the sweep.
	Enabled bool

	// Grace is how long a dispatched runner may remain unbound (never
	// seen in an in_progress event) before it is destroyed. Defaults to
	// 10 minutes when zero.
	Grace time.Duration
}

// defaultOrphanGrace is applied when OrphanSweepConfig.Grace is zero.
const defaultOrphanGrace = 10 * time.Minute

// jobKey uniquely identifies a job across providers. Different providers
// can return the same int64 job ID, so we include the provider name.
type jobKey struct {
	Provider string
	JobID    int64
}

// keyFor returns the composite job key for a given event.
func keyFor(event providers.JobEvent) jobKey {
	name := ""
	if event.Provider != nil {
		name = event.Provider.Name()
	}
	return jobKey{Provider: name, JobID: event.JobID}
}

// Scheduler ties CI provider job events to container lifecycle.
// When a job is queued, it provisions a runner environment.
// When the job completes, it destroys the environment.
type Scheduler struct {
	cfg          Config
	running      map[jobKey]*runningJob
	seen         map[jobKey]time.Time      // recently handled jobs for dedup
	pending      map[jobKey]struct{}       // jobs dispatched to a handler but not yet holding sem
	attempts     map[jobKey]int            // provisioning passes per job, for zombie detection
	runners      map[string]*runnerBinding // dispatched runners by name; tracks observed job assignment
	webhookMode  bool                      // true when job events arrive via webhooks (in_progress observable)
	mu           sync.Mutex
	sem          chan struct{} // local/native job concurrency limiter
	linuxSem     chan struct{} // Linux dispatch (VM) concurrency limiter
	macSem       chan struct{} // macOS VM concurrency limiter (Vz has a hard cap)
	nativeMacSem chan struct{} // native macOS job concurrency limiter (separate from VM limit)
	draining     bool          // true when shutting down, rejects new jobs
	startTime    time.Time

	// retry holds pending re-attempts for jobs whose initial claim
	// failed with a retryable error. Nil when Config.Retry.Enabled=false.
	retry *retryQueue
}

const seenTTL = 10 * time.Minute

// maxProvisionAttempts caps how many times a single job may be provisioned
// before it is treated as an undispatchable "zombie" and skipped.
//
// A live job reaches provisioning once: it runs to completion, GitHub marks
// it done, and it stops appearing in the queued-jobs poll. A zombie — a job
// GitHub keeps listing as queued but never actually dispatches to a runner
// (classically a workflow run superseded by a newer commit on a workflow
// without concurrency:cancel-in-progress) — reappears every seenTTL and
// would otherwise re-provision a full runner/VM forever. Since the seen
// dedup lets a given job past provisioning only ~once per seenTTL, this is
// ~maxProvisionAttempts * seenTTL (~50 min) of retries before giving up.
const maxProvisionAttempts = 5

// SetMacOSVMConfig enables macOS job support after startup. This is used when
// the macOS disk image is being provisioned in the background — the scheduler
// starts immediately for Linux jobs and picks up macOS jobs once the install
// finishes.
func (s *Scheduler) SetMacOSVMConfig(cfg *vm.MacOSVMConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.MacOSVMConfig = cfg
}

// failureBackoff tracks per-repo failure counts to compute exponential backoff.
// Resets to zero on the next successful job for that repo.
var (
	failureCounts   = map[string]int{}
	failureCountsMu sync.Mutex
)

// backoffDuration returns an exponential backoff duration based on consecutive
// failure count: 2s, 4s, 8s, 16s, 32s, capped at 60s.
func backoffDuration(repo string) time.Duration {
	failureCountsMu.Lock()
	failureCounts[repo]++
	n := failureCounts[repo]
	failureCountsMu.Unlock()

	d := time.Duration(1<<min(n, 6)) * time.Second // 2, 4, 8, 16, 32, 64
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

func resetBackoff(repo string) {
	failureCountsMu.Lock()
	delete(failureCounts, repo)
	failureCountsMu.Unlock()
}

type runningJob struct {
	env          *runtime.RunnerEnv
	provider     providers.Provider // which provider owns this job (for ReleaseJob on shutdown)
	claim        *providers.Claim   // tracks the provider claim for cleanup (ReleaseJob)
	repo         string
	image        string
	cancel       context.CancelFunc
	artifactsDir string              // non-empty if OCI artifacts were extracted for this job
	dispatched   string              // non-empty if dispatched to Linux VM worker (stores container name)
	macosVM      vm.MacOSVM          // non-nil if running as a macOS VM job
	nativeRunner interface{ Stop() } // non-nil if running as a native macOS job
	startedAt    time.Time
}

// runnerName returns the name the runner was registered under with the
// provider. Every provisioning path stores it on the claim; the Linux
// dispatch path mirrors it in dispatched.
func (rj *runningJob) runnerName() string {
	if rj.dispatched != "" {
		return rj.dispatched
	}
	if rj.claim != nil {
		return rj.claim.RunnerName
	}
	return ""
}

// runnerBinding tracks which job a dispatched runner ACTUALLY picked up.
//
// ephemerd registers one JIT runner per queued job, but the platform's
// scheduler does not honor that pairing: GitHub hands a registered
// runner ANY queued job with matching labels. When several same-label
// jobs queue at once (every multi-job workflow run), the runner
// dispatched "for" job A routinely ends up running job B. Teardown must
// therefore be keyed on the observed assignment (in_progress /
// completed runner_name), not the dispatch intent — destroying
// job.dispatched when job A completes kills whatever job the runner is
// actually executing.
type runnerBinding struct {
	intentKey    jobKey    // job the runner was dispatched for (key into s.running)
	boundKey     jobKey    // job the platform assigned (valid when bound)
	bound        bool      // true once an in_progress event named this runner
	dispatchedAt time.Time // when the runner was provisioned (orphan sweep)
	observable   bool      // provider reports runner assignments (RunnerNameReporter)
}

// trackRunning files a provisioned runner's bookkeeping under its
// dispatch-intent job key and opens a runner-name ledger entry so
// in_progress / completed events can locate the runner by NAME
// regardless of which job the platform actually assigned to it.
func (s *Scheduler) trackRunning(key jobKey, rj *runningJob, provider providers.Provider) {
	observable := false
	if rnr, ok := provider.(providers.RunnerNameReporter); ok {
		observable = rnr.ReportsRunnerNames()
	}
	s.mu.Lock()
	s.running[key] = rj
	if name := rj.runnerName(); name != "" {
		s.runners[name] = &runnerBinding{
			intentKey:    key,
			dispatchedAt: time.Now(),
			observable:   observable,
		}
	}
	s.mu.Unlock()
	metrics.JobsActive.Inc()
}

// untrackRunningLocked removes a runner's bookkeeping (running entry +
// runner-name ledger). Caller holds s.mu and is responsible for the
// actual teardown of the runner's resources.
func (s *Scheduler) untrackRunningLocked(key jobKey, rj *runningJob) {
	delete(s.running, key)
	if name := rj.runnerName(); name != "" {
		delete(s.runners, name)
	}
}

// New creates a scheduler.
func New(cfg Config) *Scheduler {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.WebhookPort <= 0 {
		cfg.WebhookPort = 8080
	}

	macVMs := cfg.MaxMacOSVMs
	if macVMs <= 0 {
		// Auto-detect: Vz allows roughly (host CPUs / CPUs-per-VM) VMs total.
		// Subtract 1 for the always-running Linux VM on darwin hosts.
		hostCPUs := goruntime.NumCPU()
		cpusPerVM := 4 // default from MacOSVMConfig.SetDefaults
		if cfg.MacOSVMConfig != nil && cfg.MacOSVMConfig.CPUs > 0 {
			cpusPerVM = int(cfg.MacOSVMConfig.CPUs)
		}
		macVMs = hostCPUs/cpusPerVM - 1 // -1 for Linux VM
		if macVMs < 1 {
			macVMs = 1
		}
	}

	nativeMac := cfg.MaxNativeMac
	if nativeMac <= 0 {
		nativeMac = 4
	}

	s := &Scheduler{
		cfg:          cfg,
		running:      make(map[jobKey]*runningJob),
		seen:         make(map[jobKey]time.Time),
		pending:      make(map[jobKey]struct{}),
		attempts:     make(map[jobKey]int),
		runners:      make(map[string]*runnerBinding),
		sem:          make(chan struct{}, cfg.MaxConcurrent),
		linuxSem:     make(chan struct{}, cfg.MaxConcurrent),
		macSem:       make(chan struct{}, macVMs),
		nativeMacSem: make(chan struct{}, nativeMac),
		startTime:    time.Now(),
	}
	// Only construct the retry queue when the caller explicitly enabled
	// it. A disabled queue is safe to leave nil; enqueueRetryIfEligible
	// nil-checks so the "log and drop" path is a no-op for opted-out
	// callers.
	if cfg.Retry.Enabled {
		log := cfg.Log
		if log == nil {
			log = slog.Default()
		}
		s.retry = newRetryQueue(cfg.Retry, log.With("component", "retry_queue"))
	}
	return s
}

// Run starts the scheduler. It discovers jobs via polling (default) or
// webhooks (when TLS certs are configured), and manages runner lifecycle.
func (s *Scheduler) Run(ctx context.Context) error {
	events := make(chan providers.JobEvent, 32)

	// Set static metrics
	metrics.ConcurrentCapacity.Set(float64(s.cfg.MaxConcurrent))

	// Update uptime periodically
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				metrics.UptimeSeconds.Set(time.Since(s.startTime).Seconds())
			case <-ctx.Done():
				return
			}
		}
	}()

	// Drive the claim/provision retry queue if configured. Nil-safe when
	// Retry.Enabled is false.
	if s.retry != nil {
		go s.retry.Run(ctx)
	}

	// Clean old job logs on startup, then periodically every hour.
	// Retention period is configurable via [log] log_retention (default 7d).
	logDir := filepath.Join(s.cfg.DataDir, "logs")
	logMaxAge := s.cfg.LogRetention
	if logMaxAge <= 0 {
		logMaxAge = 7 * 24 * time.Hour
	}
	runtime.CleanOldLogs(logDir, logMaxAge, s.cfg.Log)
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				runtime.CleanOldLogs(logDir, logMaxAge, s.cfg.Log)
			case <-ctx.Done():
				return
			}
		}
	}()

	// Start gRPC control server on unix socket
	grpcCleanup, err := s.startControlServer()
	if err != nil {
		return fmt.Errorf("starting control server: %w", err)
	}
	defer grpcCleanup()

	// Start VM SSH info server on a second unix socket (HTTP/JSON).
	// Used by `ephemerd jobs ssh <id>` to get the ephemeral key + VM IP.
	sshCleanup, err := s.StartVMSSHServer()
	if err != nil {
		s.cfg.Log.Warn("failed to start VM SSH info server", "error", err)
	} else {
		defer sshCleanup()
	}

	// Start health/webhook HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)

	// Determine job discovery mode: webhook if tunnel or secret is set, polling otherwise
	useWebhook := s.cfg.Tunnel != nil || s.cfg.WebhookSecret != ""
	useTLS := s.cfg.TLSCert != "" && s.cfg.TLSKey != ""

	// Collect webhook-capable providers and mount per-provider webhook paths.
	var whProviders []providers.Webhook
	if useWebhook {
		for _, p := range s.cfg.Providers {
			whp, ok := p.(providers.Webhook)
			if !ok {
				continue
			}
			whProviders = append(whProviders, whp)
			path := "/webhook/" + p.Name()
			handler, webhookEvents := whp.WebhookHandler(s.cfg.WebhookSecret)
			mux.Handle(path, handler)

			go func(ch <-chan providers.JobEvent) {
				for ev := range ch {
					events <- ev
				}
			}(webhookEvents)

			s.cfg.Log.Info("webhook handler registered", "provider", p.Name(), "path", path)
		}
		if len(whProviders) == 0 {
			s.cfg.Log.Warn("webhook mode requested but no provider supports webhooks, falling back to polling")
			useWebhook = false
		}
	}

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.cfg.WebhookPort),
		Handler: mux,
	}

	// Start HTTP server: via tunnel, TLS, or plain HTTP
	if s.cfg.Tunnel != nil && useWebhook {
		// Clean up stale webhooks from previous crashed instances before
		// registering new ones. Prevents hitting platform per-repo/org hook
		// limits (e.g. GitHub's 20-hook cap). Providers that don't implement
		// this are skipped.
		for _, whp := range whProviders {
			if cleaner, ok := whp.(interface{ CleanStaleWebhooks(context.Context) }); ok {
				cleaner.CleanStaleWebhooks(ctx)
			}
		}

		// Initial tunnel connection.
		ln, err := s.cfg.Tunnel.Listen(ctx)
		if err != nil {
			return fmt.Errorf("starting webhook tunnel: %w", err)
		}

		// Register webhooks for each webhook-capable provider.
		for _, whp := range whProviders {
			webhookURL := s.cfg.Tunnel.PublicURL() + "/webhook/" + whp.(providers.Provider).Name()
			s.cfg.Log.Info("webhook tunnel ready", "provider", whp.(providers.Provider).Name(), "url", webhookURL)
			if err := whp.RegisterWebhooks(ctx, webhookURL, s.cfg.WebhookSecret); err != nil {
				return fmt.Errorf("registering webhooks for %s: %w", whp.(providers.Provider).Name(), err)
			}
		}

		// Serve with automatic reconnect on tunnel drops.
		// serveTunnelWithReconnect owns the full lifecycle: it creates
		// fresh HTTP servers on each reconnect, closes old listeners,
		// and deregisters webhooks on shutdown. No defer needed here.
		go s.serveTunnelWithReconnect(ctx, mux, ln, whProviders, events)
	} else if useTLS {
		go func() {
			s.cfg.Log.Info("webhook server listening (TLS)", "port", s.cfg.WebhookPort)
			if err := server.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey); err != http.ErrServerClosed {
				s.cfg.Log.Error("webhook server error", "error", err)
			}
		}()
	} else {
		go func() {
			if useWebhook {
				s.cfg.Log.Info("webhook server listening (HTTP)", "port", s.cfg.WebhookPort)
			} else {
				s.cfg.Log.Info("health server listening", "port", s.cfg.WebhookPort)
			}
			if err := server.ListenAndServe(); err != http.ErrServerClosed {
				s.cfg.Log.Error("server error", "error", err)
			}
		}()
	}
	defer func() { _ = server.Shutdown(context.Background()) }()

	// Start polling for all poll-capable providers (those not using webhooks,
	// or all of them when webhook mode is off).
	if !useWebhook {
		interval := s.cfg.PollInterval
		if interval <= 0 {
			interval = 10 * time.Second
		}
		started := 0
		for _, p := range s.cfg.Providers {
			pollProvider, ok := p.(providers.Poll)
			if !ok {
				s.cfg.Log.Warn("provider does not support polling, skipping", "provider", p.Name())
				continue
			}
			pollCh, err := pollProvider.Start(ctx, providers.PollConfig{
				PollInterval: int(interval.Seconds()),
			})
			if err != nil {
				s.cfg.Log.Error("failed to start poll provider", "provider", p.Name(), "error", err)
				continue
			}
			go func(ch <-chan providers.JobEvent) {
				for ev := range ch {
					events <- ev
				}
			}(pollCh)
			s.cfg.Log.Info("polling started", "provider", p.Name(), "interval", interval)
			started++
		}
		if started == 0 {
			return fmt.Errorf("no providers started successfully")
		}
	}

	// One-time poll on startup to catch jobs that queued while ephemerd
	// was down. Webhook events only fire at the moment a job transitions
	// to "queued" — they aren't replayed for jobs already in that state.
	// Continuous-poll mode catches these on the next tick naturally, but
	// in webhook mode we need an explicit one-shot. Run in a goroutine so
	// it doesn't block if there are more queued jobs than the channel buffer.
	for _, p := range s.cfg.Providers {
		catcher, ok := p.(interface{ CatchUpPoll(context.Context) error })
		if !ok {
			continue
		}
		name := p.Name()
		s.cfg.Log.Info("startup poll: checking for queued jobs", "provider", name)
		go func() {
			if err := catcher.CatchUpPoll(ctx); err != nil {
				s.cfg.Log.Warn("startup poll failed", "provider", name, "error", err)
			}
		}()
	}

	// Record the discovery mode: the orphaned-runner sweep is only safe
	// when in_progress events are observable, i.e. in webhook mode.
	s.mu.Lock()
	s.webhookMode = useWebhook
	s.mu.Unlock()

	// Periodically clean up the seen-jobs dedup map and sweep orphaned
	// runners (dispatched but never assigned a job by the platform).
	cleanupTicker := time.NewTicker(5 * time.Minute)
	defer cleanupTicker.Stop()

	// Process events from all providers via the unified events channel.
	for {
		select {
		case <-cleanupTicker.C:
			s.cleanSeen()
			s.sweepOrphanRunners()

		case <-ctx.Done():
			s.cfg.Log.Info("shutting down scheduler")
			s.drain()
			return nil

		case event := <-events:
			switch event.Action {
			case "queued":
				metrics.JobsQueuedTotal.Inc()
				go s.handleQueued(ctx, event)
			case "in_progress":
				s.handleInProgress(event)
			case "completed":
				go s.handleCompleted(ctx, event)
			}
		}
	}
}

// canHandleJob returns false if the job's labels include an OS or
// architecture that this scheduler cannot handle.
func (s *Scheduler) canHandleJob(jobLabels []string) bool {
	osOK := true // assume OK until we see an OS label we can't handle
	for _, label := range jobLabels {
		switch strings.ToLower(label) {
		case "linux":
			// Linux jobs run natively on Linux, via VM dispatch on Windows/macOS,
			// or inside the embedded Linux VM on macOS.
			osOK = goruntime.GOOS == "linux" || goruntime.GOOS == "darwin" || s.cfg.LinuxDispatcher != nil
		case "windows":
			osOK = goruntime.GOOS == "windows"
		case "macos", "macosx":
			// macOS jobs run in a per-job VM (default) or natively on
			// the host (when configured for trusted repos). Accept if
			// either VM config or native mode is available.
			osOK = goruntime.GOOS == "darwin" && (s.cfg.MacOSVMConfig != nil || s.cfg.MacOSModeForRepo != nil)
		}
	}
	if !osOK {
		return false
	}
	// Arch check: if the job asks for an arch we can't satisfy, skip. We
	// don't emulate (no qemu-user, no rosetta-in-container), so x64 jobs
	// on an arm64 host and vice versa won't work.
	for _, label := range jobLabels {
		switch strings.ToLower(label) {
		case "x64", "amd64":
			if goruntime.GOARCH != "amd64" {
				return false
			}
		case "arm64", "aarch64":
			if goruntime.GOARCH != "arm64" {
				return false
			}
		}
	}
	return true
}

// isLinuxJob returns true if the job's labels include "linux".
func isLinuxJob(labels []string) bool {
	for _, label := range labels {
		if strings.ToLower(label) == "linux" {
			return true
		}
	}
	return false
}

// isMacOSJob returns true if the job's labels include a macOS identifier.
func isMacOSJob(labels []string) bool {
	for _, label := range labels {
		switch strings.ToLower(label) {
		case "macos", "macosx":
			return true
		}
		if strings.HasPrefix(strings.ToLower(label), "macos-") {
			return true
		}
	}
	return false
}

func (s *Scheduler) handleQueued(ctx context.Context, event providers.JobEvent) {
	jobID := event.JobID
	key := keyFor(event)
	log := s.cfg.Log.With("job_id", jobID, "repo", event.Repo)

	// Skip jobs whose OS labels don't match this platform
	if len(event.Labels) > 0 && !s.canHandleJob(event.Labels) {
		log.Debug("skipping job, OS labels don't match this platform", "labels", event.Labels)
		return
	}

	// Dedup: skip if we've already seen this job recently
	s.mu.Lock()
	if _, exists := s.running[key]; exists {
		s.mu.Unlock()
		log.Debug("ignoring duplicate queued event, job already running")
		return
	}
	if _, exists := s.pending[key]; exists {
		s.mu.Unlock()
		log.Debug("ignoring duplicate queued event, job pending semaphore")
		return
	}
	if t, seen := s.seen[key]; seen && time.Since(t) < seenTTL {
		s.mu.Unlock()
		log.Debug("ignoring duplicate queued event, job recently handled")
		return
	}
	s.pending[key] = struct{}{}
	s.seen[key] = time.Now()

	// Zombie guard: a job that keeps reaching provisioning but never runs to
	// completion (GitHub lists it queued but never dispatches it) is skipped
	// after maxProvisionAttempts so it stops re-provisioning a runner/VM on
	// every seenTTL. The counter is pruned in cleanSeen once the job stops
	// appearing (GitHub finished/cancelled it), so a later legitimate rerun
	// starts fresh.
	s.attempts[key]++
	if s.attempts[key] > maxProvisionAttempts {
		delete(s.pending, key)
		attempts := s.attempts[key]
		s.mu.Unlock()
		// Warn once when first crossing the cap; stay quiet on later polls.
		if attempts == maxProvisionAttempts+1 {
			log.Warn("job repeatedly provisioned but never ran to completion — treating as undispatchable (zombie) and skipping",
				"attempts", maxProvisionAttempts,
				"hint", "workflow run is likely superseded; cancel it or add concurrency:cancel-in-progress")
		} else {
			log.Debug("skipping zombie job (already over provision cap)", "attempts", attempts)
		}
		return
	}

	if s.draining {
		s.mu.Unlock()
		log.Info("rejecting job, scheduler is draining")
		return
	}
	s.mu.Unlock()

	// Dispatch Linux jobs to the Linux VM worker if available
	if s.cfg.LinuxDispatcher != nil && isLinuxJob(event.Labels) {
		s.handleLinuxJob(ctx, event)
		return
	}

	// Route macOS jobs to native runner or per-job VM.
	if isMacOSJob(event.Labels) {
		// Native mode takes priority when configured for this repo
		if s.cfg.MacOSModeForRepo != nil && s.cfg.MacOSModeForRepo(event.Repo) == "native" {
			s.handleNativeMacOSJob(ctx, event)
			return
		}
		// VM path
		s.mu.Lock()
		macCfg := s.cfg.MacOSVMConfig
		s.mu.Unlock()
		if macCfg != nil {
			s.handleMacOSJob(ctx, event)
			return
		}
		// Neither native nor VM available — remove from seen/pending
		// so the next poll retries this job once the install finishes.
		s.mu.Lock()
		delete(s.seen, key)
		delete(s.pending, key)
		s.mu.Unlock()
		log.Info("macOS runner not ready, deferring job")
		return
	}

	s.handleLocalJob(ctx, event)
}

// handleLinuxJob dispatches a Linux job to the Linux VM worker via gRPC.
// The host registers the JIT runner (with Linux labels) and sends
// Create/Wait/Destroy RPCs to the dispatch server running inside the VM
// (WSL on Windows, Virtualization.framework on macOS).
func (s *Scheduler) handleLinuxJob(ctx context.Context, event providers.JobEvent) {
	jobID := event.JobID
	key := keyFor(event)
	log := s.cfg.Log.With("job_id", jobID, "repo", event.Repo, "dispatch", "linux")

	unsee := func() {
		s.mu.Lock()
		delete(s.seen, key)
		delete(s.pending, key)
		s.mu.Unlock()
	}

	// Acquire Linux dispatch concurrency slot (separate from local/macOS)
	select {
	case s.linuxSem <- struct{}{}:
	case <-ctx.Done():
		unsee()
		return
	}
	s.mu.Lock()
	delete(s.pending, key)
	s.mu.Unlock()

	log.Info("provisioning Linux runner via dispatch")

	image := s.resolveImage(ctx, &event, "linux")
	if image != "" {
		log.Info("using image for job", "image", image, "repo", event.Repo)
	}

	labels := buildLabelsForOS("linux", s.cfg.Labels)

	const maxNameRetries = 3
	claim, err := s.claimJob(ctx, &event, labels, log, maxNameRetries)
	if err != nil {
		log.Error("failed to claim job", "error", err, "error_class", classifyErr(err))
		unsee()
		<-s.linuxSem
		// Replaces the old blind time.Sleep(backoffDuration): the
		// sem is released FIRST so we do not hold a slot idle across
		// the wait, then a rate-aware jittered retry is enqueued
		// (no-op when Retry.Enabled is false or the error is not
		// retryable).
		s.enqueueRetryIfEligible(ctx, event, err)
		return
	}

	var jobCtx context.Context
	var cancel context.CancelFunc
	if s.cfg.JobTimeout > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, s.cfg.JobTimeout)
	} else {
		jobCtx, cancel = context.WithCancel(ctx)
	}

	if err := s.cfg.LinuxDispatcher.Create(jobCtx, claim.RunnerName, image, claim.RunnerConfig, event.Provider.Name(), event.Repo); err != nil {
		log.Error("dispatch create failed", "error", err)
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		unsee()
		cancel()
		<-s.linuxSem
		return
	}

	// Track the dispatched job (env is nil — lifecycle managed by Linux VM worker)
	s.trackRunning(key, &runningJob{
		provider:   event.Provider,
		claim:      claim,
		repo:       event.Repo,
		image:      image,
		cancel:     cancel,
		dispatched: claim.RunnerName,
		startedAt:  time.Now(),
	}, event.Provider)

	log.Info("Linux runner dispatched", "name", claim.RunnerName)

	// Wait for the job to finish in the background
	go func() {
		defer func() { <-s.linuxSem }()

		exitCode, err := s.cfg.LinuxDispatcher.Wait(jobCtx, claim.RunnerName)
		if err != nil {
			if jobCtx.Err() != nil {
				log.Warn("dispatched runner killed (timeout or shutdown)", "error", err)
			} else {
				log.Error("dispatched runner wait failed", "error", err)
			}
		} else if exitCode != 0 {
			log.Warn("dispatched runner exited with failure", "exit_code", exitCode)
		} else {
			log.Info("dispatched runner exited", "exit_code", exitCode)
		}

		// Always clean up. The entry may already be gone if a completed
		// event tore this runner down by name (see handleCompleted).
		s.mu.Lock()
		rj, exists := s.running[key]
		if exists {
			s.untrackRunningLocked(key, rj)
		}
		s.mu.Unlock()
		if exists {
			metrics.JobsActive.Dec()
		}

		if err := s.cfg.LinuxDispatcher.Destroy(context.Background(), claim.RunnerName); err != nil {
			log.Warn("dispatch destroy failed", "error", err)
		}

		// Deregister the runner from the provider so it doesn't linger as
		// offline on GitHub. On normal completion the provider may have
		// already removed it (JIT runners auto-remove), but the call is
		// idempotent — a 404 just means it's already gone.
		if exists && rj.provider != nil && rj.claim != nil {
			if err := rj.provider.ReleaseJob(context.Background(), rj.claim); err != nil {
				log.Debug("deregister runner after dispatch cleanup", "error", err)
			}
		}
	}()
}

// handleMacOSJob provisions a per-job macOS VM via Virtualization.framework.
// The base image must have the GitHub Actions runner pre-installed. The JIT
// config is passed via a virtio-fs shared directory.
func (s *Scheduler) handleMacOSJob(ctx context.Context, event providers.JobEvent) {
	jobID := event.JobID
	key := keyFor(event)
	log := s.cfg.Log.With("job_id", jobID, "repo", event.Repo, "platform", "macos")

	unsee := func() {
		s.mu.Lock()
		delete(s.seen, key)
		delete(s.pending, key)
		s.mu.Unlock()
	}

	// Acquire macOS VM concurrency slot (separate from Linux/local sem).
	select {
	case s.macSem <- struct{}{}:
	case <-ctx.Done():
		unsee()
		return
	}
	s.mu.Lock()
	delete(s.pending, key)
	s.mu.Unlock()

	log.Info("provisioning macOS VM runner for job")

	// Extract OCI artifacts if an image is specified
	image := s.resolveImage(ctx, &event, "darwin")
	var artifactsDir string
	if image != "" && s.cfg.Artifacts != nil {
		artifactsDir = artifacts.ArtifactsDir(s.cfg.DataDir, fmt.Sprintf("%d", jobID))
		log.Info("extracting OCI artifacts for macOS VM job", "image", image, "dest", artifactsDir)
		if err := s.cfg.Artifacts.Extract(ctx, image, artifactsDir); err != nil {
			log.Error("failed to extract OCI artifacts", "image", image, "error", err)
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
			artifactsDir = ""
		}
	}

	// Claim job with macOS labels
	labels := buildLabelsForOS("darwin", s.cfg.Labels)
	const maxNameRetries = 3
	claim, err := s.claimJob(ctx, &event, labels, log, maxNameRetries)
	if err != nil {
		log.Error("failed to claim job", "error", err, "error_class", classifyErr(err))
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		unsee()
		<-s.macSem
		s.enqueueRetryIfEligible(ctx, event, err)
		return
	}

	// Create the macOS VM
	macVM, err := vm.NewMacOSVM(*s.cfg.MacOSVMConfig, fmt.Sprintf("%d", jobID))
	if err != nil {
		log.Error("failed to create macOS VM", "error", err)
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		unsee()
		<-s.macSem
		return
	}

	// Write JIT config to the shared directory before booting
	if err := macVM.WriteJITConfig(claim.RunnerConfig); err != nil {
		log.Error("failed to write JIT config", "error", err)
		macVM.Stop()
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		unsee()
		<-s.macSem
		return
	}

	var jobCtx context.Context
	var cancel context.CancelFunc
	if s.cfg.JobTimeout > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, s.cfg.JobTimeout)
	} else {
		jobCtx, cancel = context.WithCancel(ctx)
	}

	// Boot the VM
	if err := macVM.Start(jobCtx); err != nil {
		log.Error("failed to start macOS VM", "error", err)
		macVM.Stop()
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		unsee()
		cancel()
		<-s.macSem
		return
	}

	// Wait for the runner inside the VM to become reachable
	ip, err := macVM.WaitForRunner(jobCtx)
	if err != nil {
		log.Error("macOS VM runner not reachable", "error", err)
		macVM.Stop()
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		unsee()
		cancel()
		<-s.macSem
		return
	}

	// Track the running job
	s.trackRunning(key, &runningJob{
		provider:     event.Provider,
		claim:        claim,
		repo:         event.Repo,
		image:        image,
		cancel:       cancel,
		artifactsDir: artifactsDir,
		macosVM:      macVM,
		startedAt:    time.Now(),
	}, event.Provider)

	log.Info("macOS VM runner ready", "name", claim.RunnerName, "ip", ip)

	// Wait for the job to finish in the background
	go func() {
		defer func() { <-s.macSem }()

		exitCode, err := macVM.Wait(jobCtx)
		if err != nil {
			if jobCtx.Err() != nil {
				log.Warn("macOS VM killed (timeout or shutdown)", "error", err)
			} else {
				log.Error("macOS VM crashed", "error", err)
			}
		} else if exitCode != 0 {
			log.Warn("macOS VM exited with failure", "exit_code", exitCode)
		} else {
			log.Info("macOS VM exited", "exit_code", exitCode)
		}

		// Clean up
		s.mu.Lock()
		rj, exists := s.running[key]
		if exists {
			s.untrackRunningLocked(key, rj)
			s.mu.Unlock()
			metrics.JobsActive.Dec()
			rj.macosVM.Stop()
			if rj.artifactsDir != "" {
				artifacts.Cleanup(rj.artifactsDir, s.cfg.Log)
			}
			if rj.provider != nil && rj.claim != nil {
				if err := rj.provider.ReleaseJob(context.Background(), rj.claim); err != nil {
					log.Debug("deregister runner after macOS VM cleanup", "error", err)
				}
			}
		} else {
			s.mu.Unlock()
		}
	}()
}

// handleNativeMacOSJob runs the GitHub Actions runner directly on the macOS
// host inside a sandbox. Used for trusted repos that don't need VM isolation.
func (s *Scheduler) handleNativeMacOSJob(ctx context.Context, event providers.JobEvent) {
	jobID := event.JobID
	key := keyFor(event)
	log := s.cfg.Log.With("job_id", jobID, "repo", event.Repo, "platform", "macos-native")

	unsee := func() {
		s.mu.Lock()
		delete(s.seen, key)
		delete(s.pending, key)
		s.mu.Unlock()
	}

	// Acquire native macOS concurrency slot (separate from VM sem)
	select {
	case s.nativeMacSem <- struct{}{}:
	case <-ctx.Done():
		unsee()
		return
	}
	s.mu.Lock()
	delete(s.pending, key)
	s.mu.Unlock()

	log.Info("provisioning native macOS runner for job")

	// Claim job with macOS labels
	labels := buildLabelsForOS("darwin", s.cfg.Labels)
	const maxNameRetries = 3
	claim, err := s.claimJob(ctx, &event, labels, log, maxNameRetries)
	if err != nil {
		log.Error("failed to claim job", "error", err, "error_class", classifyErr(err))
		unsee()
		s.enqueueRetryIfEligible(ctx, event, err)
		time.Sleep(backoffDuration(event.Repo))
		<-s.nativeMacSem
		return
	}

	// Create the native runner
	nr, err := native.New(s.cfg.DataDir, fmt.Sprintf("%d", jobID), claim.RunnerConfig, s.cfg.RunnerDir, s.cfg.PrivateKeyPath, log)
	if err != nil {
		log.Error("failed to create native runner", "error", err)
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		unsee()
		<-s.nativeMacSem
		return
	}
	if s.cfg.NativeMacUser != "" {
		nr.SetRunAsUser(s.cfg.NativeMacUser)
	}

	var jobCtx context.Context
	var cancel context.CancelFunc
	if s.cfg.JobTimeout > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, s.cfg.JobTimeout)
	} else {
		jobCtx, cancel = context.WithCancel(ctx)
	}

	// Start the runner
	if err := nr.Start(jobCtx); err != nil {
		log.Error("failed to start native runner", "error", err)
		nr.Stop()
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		unsee()
		cancel()
		<-s.nativeMacSem
		return
	}

	// Track the running job
	s.trackRunning(key, &runningJob{
		provider:     event.Provider,
		claim:        claim,
		repo:         event.Repo,
		cancel:       cancel,
		nativeRunner: nr,
		startedAt:    time.Now(),
	}, event.Provider)

	log.Info("native macOS runner started", "name", claim.RunnerName)

	// Wait for the job to finish in the background
	go func() {
		defer func() { <-s.nativeMacSem }()

		exitCode, err := nr.Wait()
		if err != nil {
			if jobCtx.Err() != nil {
				log.Warn("native macOS runner killed (timeout or shutdown)", "error", err)
			} else {
				log.Error("native macOS runner crashed", "error", err)
			}
		} else if exitCode != 0 {
			log.Warn("native macOS runner exited with failure", "exit_code", exitCode)
		} else {
			log.Info("native macOS runner exited", "exit_code", exitCode)
		}

		// Clean up
		s.mu.Lock()
		rj, exists := s.running[key]
		if exists {
			s.untrackRunningLocked(key, rj)
			s.mu.Unlock()
			metrics.JobsActive.Dec()
			nr.Stop()
			if rj.provider != nil && rj.claim != nil {
				if err := rj.provider.ReleaseJob(context.Background(), rj.claim); err != nil {
					log.Debug("deregister runner after native macOS cleanup", "error", err)
				}
			}
		} else {
			s.mu.Unlock()
		}
	}()
}

// handleLocalJob provisions a runner using the local containerd Runtime.
func (s *Scheduler) handleLocalJob(ctx context.Context, event providers.JobEvent) {
	jobID := event.JobID
	key := keyFor(event)
	log := s.cfg.Log.With("job_id", jobID, "repo", event.Repo)

	// On provisioning failure, remove from seen/pending so the next poll retries
	unsee := func() {
		s.mu.Lock()
		delete(s.seen, key)
		delete(s.pending, key)
		s.mu.Unlock()
	}

	// Acquire concurrency slot
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		unsee()
		return
	}
	s.mu.Lock()
	delete(s.pending, key)
	s.mu.Unlock()

	log.Info("provisioning runner for job")

	// Resolve image for this job. Order:
	//   1. workflow YAML (FetchJobImage)
	//   2. [runner.images.<repo>].<os> override
	//   3. provider per-OS default (DefaultImageFor)
	//   4. empty → runtime.Create picks host-aware fallback (servercore on Windows)
	jobOS := "linux"
	switch {
	case isMacOSJob(event.Labels):
		jobOS = "macos"
	case !isLinuxJob(event.Labels) && goruntime.GOOS == "windows":
		jobOS = "windows"
	}
	image := s.resolveImage(ctx, &event, jobOS)
	if image != "" {
		log.Info("using image for job", "image", image, "os", jobOS, "repo", event.Repo)
	}

	// For macOS VM jobs with an OCI image specified, extract artifact layers
	// into the shared data directory so they're available inside the VM via virtio-fs.
	var artifactsDir string
	if image != "" && s.cfg.Artifacts != nil && goruntime.GOOS == "darwin" {
		artifactsDir = artifacts.ArtifactsDir(s.cfg.DataDir, fmt.Sprintf("%d", jobID))
		log.Info("extracting OCI artifacts for macOS VM job", "image", image, "dest", artifactsDir)
		if err := s.cfg.Artifacts.Extract(ctx, image, artifactsDir); err != nil {
			log.Error("failed to extract OCI artifacts", "image", image, "error", err)
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
			artifactsDir = ""
			// Non-fatal: the job can still run without pre-extracted artifacts
		} else {
			log.Info("OCI artifacts ready for macOS VM", "dest", artifactsDir)
		}
	}

	// Build runner labels. When the job requests a specific OS (e.g. `linux`)
	// we must register the runner with matching labels or the provider won't
	// route the job to us — even if we can execute it. On Darwin the host OS
	// is `darwin` but we run `linux` jobs inside the embedded Linux VM, so
	// honour the job's labels rather than blindly using the host.
	var targetOS string
	switch {
	case isLinuxJob(event.Labels):
		targetOS = "linux"
	case isMacOSJob(event.Labels):
		targetOS = "darwin"
	default:
		targetOS = goruntime.GOOS
	}
	labels := buildLabelsForOS(targetOS, s.cfg.Labels)

	// Claim the job with a unique runner name.
	// Retry with a new name on 409 conflict (stale runner from a previous crash).
	const maxNameRetries = 3
	claim, err := s.claimJob(ctx, &event, labels, log, maxNameRetries)
	if err != nil {
		log.Error("failed to claim job", "error", err, "error_class", classifyErr(err))
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		unsee()
		<-s.sem
		// Replaces the old blind 5s sleep with a rate-aware jittered
		// retry. No-op when Retry is disabled or the error is not
		// retryable.
		s.enqueueRetryIfEligible(ctx, event, err)
		return
	}

	// Create the runner environment with job timeout
	var jobCtx context.Context
	var cancel context.CancelFunc
	if s.cfg.JobTimeout > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, s.cfg.JobTimeout)
	} else {
		jobCtx, cancel = context.WithCancel(ctx)
	}
	env, err := s.cfg.Runtime.Create(jobCtx, runtime.CreateConfig{
		ID:         claim.RunnerName,
		Image:      image,
		JITConfig:  claim.RunnerConfig,
		Env:        claim.Env,
		Entrypoint: claim.Entrypoint,
	})
	if err != nil {
		log.Error("failed to create runner environment", "error", err)
		// Remove the ghost runner since the container won't start
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		unsee()
		cancel()
		<-s.sem
		return
	}

	// Track the running job
	s.trackRunning(key, &runningJob{
		env:          env,
		provider:     event.Provider,
		claim:        claim,
		repo:         event.Repo,
		image:        image,
		cancel:       cancel,
		artifactsDir: artifactsDir,
		startedAt:    time.Now(),
	}, event.Provider)

	log.Info("runner environment ready", "name", claim.RunnerName)

	// Wait for the job to finish in the background
	go func() {
		defer func() {
			<-s.sem // release concurrency slot
		}()

		exitCode, err := s.cfg.Runtime.Wait(jobCtx, env)
		if err != nil {
			if jobCtx.Err() != nil {
				log.Warn("runner killed (timeout or shutdown)", "error", err)
			} else {
				log.Error("runner crashed", "error", err)
			}
		} else if exitCode == 137 {
			log.Warn("runner killed by OOM or signal", "exit_code", exitCode)
		} else if exitCode != 0 {
			log.Warn("runner exited with failure", "exit_code", exitCode)
		} else {
			log.Info("runner exited", "exit_code", exitCode)
		}

		// Always clean up — whether normal exit, crash, OOM, or timeout
		s.mu.Lock()
		rj, exists := s.running[key]
		if exists {
			s.untrackRunningLocked(key, rj)
			s.mu.Unlock()
			metrics.JobsActive.Dec()
			if err := s.cfg.Runtime.Destroy(context.Background(), env); err != nil {
				log.Warn("failed to destroy runner environment", "error", err)
			}
			if rj.artifactsDir != "" {
				artifacts.Cleanup(rj.artifactsDir, s.cfg.Log)
			}
			if rj.provider != nil && rj.claim != nil {
				if err := rj.provider.ReleaseJob(context.Background(), rj.claim); err != nil {
					log.Debug("deregister runner after local cleanup", "error", err)
				}
			}
		} else {
			s.mu.Unlock()
		}
	}()
}

// handleInProgress records which runner the platform ACTUALLY assigned
// a job to. GitHub schedules JIT runners onto any queued job with
// matching labels, so this routinely differs from the dispatch intent;
// the binding recorded here is what keeps handleCompleted from
// destroying a runner that is mid-flight on someone else's job. It also
// drops any outstanding claim retry for the job (it's running — possibly
// on a peer daemon — so re-attempting would register ghost runners).
func (s *Scheduler) handleInProgress(event providers.JobEvent) {
	key := keyFor(event)
	if s.retry != nil {
		s.retry.Drop(key)
	}

	name := event.RunnerName
	if name == "" {
		return
	}

	s.mu.Lock()
	rb, owned := s.runners[name]
	var intentKey jobKey
	if owned {
		rb.bound = true
		rb.boundKey = key
		intentKey = rb.intentKey
	}
	s.mu.Unlock()

	if !owned {
		return
	}
	if intentKey != key {
		// The observability line for the fungibility race: GitHub gave
		// our runner a different job than the one it was dispatched for.
		s.cfg.Log.Info("runner picked up a different job than it was dispatched for",
			"runner", name,
			"dispatched_for_job", intentKey.JobID,
			"assigned_job", key.JobID,
			"repo", event.Repo,
			"detail", "GitHub treats same-label JIT runners as fungible; teardown will follow the observed assignment")
	} else {
		s.cfg.Log.Debug("runner bound to its dispatched job", "runner", name, "job_id", key.JobID)
	}
}

func (s *Scheduler) handleCompleted(ctx context.Context, event providers.JobEvent) {
	jobID := event.JobID
	key := keyFor(event)
	log := s.cfg.Log.With("job_id", jobID, "repo", event.Repo)

	// Drop any outstanding retry attempts: the provider says this job
	// is finished, so re-attempting would waste API budget and could
	// register ghost runners. Nil-safe.
	if s.retry != nil {
		s.retry.Drop(key)
	}

	// Resolve which runner to tear down. The runner that ran this job is
	// the one NAMED IN THE EVENT — not necessarily the one dispatched
	// when the job queued (GitHub reassigns same-label JIT runners
	// freely). Destroying job.dispatched here used to kill whichever job
	// the reassigned runner was actually executing.
	s.mu.Lock()
	var job *runningJob
	exists := false
	ownerKey := key
	if name := event.RunnerName; name != "" {
		if rb, owned := s.runners[name]; owned {
			if rj, ok := s.running[rb.intentKey]; ok && rj.runnerName() == name {
				job, ownerKey, exists = rj, rb.intentKey, true
			} else {
				// Ledger points at an entry the wait-goroutine already
				// cleaned up — drop the stale ledger record.
				delete(s.runners, name)
			}
		}
		// Not ours (peer daemon / foreign runner): nothing to destroy.
		// If we dispatched an intent runner for this job it is still
		// alive and unbound — it will pick up another queued job, exit
		// on its own, or be culled by the orphan sweep.
	} else if rj, ok := s.running[key]; ok {
		// No runner named in the event (job cancelled before any runner
		// picked it up, or a provider that doesn't report runner names).
		// Fall back to the dispatch-intent runner, but only when it was
		// never observed running a DIFFERENT job.
		rb := s.runners[rj.runnerName()]
		if rb == nil || !rb.bound || rb.boundKey == key {
			job, exists = rj, true
		} else {
			log.Info("job completed without a runner name; leaving dispatch-intent runner alone (it is bound to another job)",
				"runner", rj.runnerName(),
				"bound_job", rb.boundKey.JobID)
		}
	}
	if exists {
		s.untrackRunningLocked(ownerKey, job)
	}
	s.mu.Unlock()

	if !exists {
		return
	}

	conclusion := event.Conclusion
	log.Info("job completed, destroying runner environment",
		"conclusion", conclusion,
		"runner", job.runnerName(),
	)
	if ownerKey != key {
		log.Info("destroying runner under its observed assignment, not its dispatch intent",
			"runner", job.runnerName(),
			"dispatched_for_job", ownerKey.JobID)
	}

	// Record metrics
	providerName := ""
	if event.Provider != nil {
		providerName = event.Provider.Name()
	}
	metrics.JobsActive.Dec()
	metrics.JobsTotal.WithLabelValues(providerName, event.Repo, conclusion).Inc()
	metrics.JobDuration.WithLabelValues(providerName, event.Repo).Observe(time.Since(job.startedAt).Seconds())

	resetBackoff(event.Repo)
	job.cancel()
	if job.macosVM != nil {
		job.macosVM.Stop()
	} else if job.nativeRunner != nil {
		job.nativeRunner.Stop()
	} else if job.dispatched != "" && s.cfg.LinuxDispatcher != nil {
		if err := s.cfg.LinuxDispatcher.Destroy(context.Background(), job.dispatched); err != nil {
			log.Warn("failed to destroy dispatched runner", "error", err)
		}
	} else if job.env != nil {
		if err := s.cfg.Runtime.Destroy(context.Background(), job.env); err != nil {
			log.Warn("failed to destroy runner environment", "error", err)
		}
	}
	if job.artifactsDir != "" {
		artifacts.Cleanup(job.artifactsDir, s.cfg.Log)
	}
}

// drain stops accepting new jobs and waits for running jobs to finish.
// If jobs don't finish within ShutdownTimeout, they are force-killed.
func (s *Scheduler) drain() {
	s.mu.Lock()
	s.draining = true
	count := len(s.running)
	s.mu.Unlock()
	metrics.Draining.Set(1)

	if count == 0 {
		return
	}

	timeout := s.cfg.ShutdownTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	s.cfg.Log.Info("waiting for running jobs to finish", "count", count, "timeout", timeout)

	deadline := time.After(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			s.cfg.Log.Warn("shutdown timeout reached, force-killing remaining jobs")
			s.destroyAll()
			return
		case <-ticker.C:
			s.mu.Lock()
			remaining := len(s.running)
			s.mu.Unlock()
			if remaining == 0 {
				s.cfg.Log.Info("all jobs finished cleanly")
				return
			}
		}
	}
}

func (s *Scheduler) destroyAll() {
	s.mu.Lock()
	jobs := make(map[jobKey]*runningJob, len(s.running))
	for k, v := range s.running {
		jobs[k] = v
	}
	s.running = make(map[jobKey]*runningJob)
	s.runners = make(map[string]*runnerBinding)
	s.mu.Unlock()

	for key, job := range jobs {
		s.cfg.Log.Info("destroying runner on shutdown", "job_id", key.JobID, "provider", key.Provider)
		job.cancel()
		if job.macosVM != nil {
			job.macosVM.Stop()
		} else if job.nativeRunner != nil {
			job.nativeRunner.Stop()
		} else if job.dispatched != "" && s.cfg.LinuxDispatcher != nil {
			if err := s.cfg.LinuxDispatcher.Destroy(context.Background(), job.dispatched); err != nil {
				s.cfg.Log.Warn("failed to destroy dispatched runner", "job_id", key.JobID, "error", err)
			}
		} else if job.env != nil {
			if err := s.cfg.Runtime.Destroy(context.Background(), job.env); err != nil {
				s.cfg.Log.Warn("failed to destroy runner on shutdown", "job_id", key.JobID, "error", err)
			}
		}
		if job.artifactsDir != "" {
			artifacts.Cleanup(job.artifactsDir, s.cfg.Log)
		}
		// Deregister the runner from the provider to avoid ghosts
		if job.claim != nil && job.provider != nil {
			if err := job.provider.ReleaseJob(context.Background(), job.claim); err != nil {
				s.cfg.Log.Warn("failed to deregister runner on shutdown", "job_id", key.JobID, "runner_id", job.claim.RunnerID, "error", err)
			} else {
				s.cfg.Log.Info("runner deregistered", "job_id", key.JobID, "runner_id", job.claim.RunnerID)
			}
		}
	}
}

func (s *Scheduler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	activeJobs := len(s.running)
	draining := s.draining
	s.mu.Unlock()

	status := map[string]any{
		"status":         "ok",
		"active_jobs":    activeJobs,
		"max_concurrent": s.cfg.MaxConcurrent,
		"draining":       draining,
		"uptime":         time.Since(s.startTime).String(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(status); err != nil {
		s.cfg.Log.Error("failed to encode healthz response", "error", err)
	}
}

func (s *Scheduler) cleanSeen() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.seen {
		if time.Since(t) > seenTTL {
			delete(s.seen, id)
			// Job stopped appearing in the queue (finished/cancelled) — reset
			// its zombie counter so a future legitimate rerun starts fresh.
			delete(s.attempts, id)
		}
	}
}

// sweepOrphanRunners destroys dispatched runners that were never
// observed picking up a job within the configured grace window.
//
// Before teardown was keyed on observed assignments, every completed
// event destroyed the runner dispatched for that job — which implicitly
// (and often wrongly) cleaned up runners whose job was taken by a peer.
// Now that a completed event only touches the runner named in it, a
// runner whose intended job was cancelled before assignment (with no
// runner_name in the completed event and a binding elsewhere), or whose
// job was grabbed by another daemon's runner, has no event that will
// ever destroy it. The sweep is that replacement cleanup.
//
// Safety: only runs in webhook mode (in poll mode there are no
// in_progress events, so "never observed bound" is meaningless) and only
// for runners dispatched via providers that report runner assignments.
func (s *Scheduler) sweepOrphanRunners() {
	if !s.cfg.OrphanSweep.Enabled {
		return
	}
	grace := s.cfg.OrphanSweep.Grace
	if grace <= 0 {
		grace = defaultOrphanGrace
	}

	type victim struct {
		name string
		key  jobKey
		rj   *runningJob
	}
	var victims []victim

	s.mu.Lock()
	if !s.webhookMode {
		s.mu.Unlock()
		return
	}
	for name, rb := range s.runners {
		if rb.bound || !rb.observable || time.Since(rb.dispatchedAt) < grace {
			continue
		}
		rj, ok := s.running[rb.intentKey]
		if !ok {
			// Stale ledger entry — the wait-goroutine already cleaned up.
			delete(s.runners, name)
			continue
		}
		if rj.runnerName() != name {
			continue
		}
		s.untrackRunningLocked(rb.intentKey, rj)
		victims = append(victims, victim{name: name, key: rb.intentKey, rj: rj})
	}
	s.mu.Unlock()

	for _, v := range victims {
		s.cfg.Log.Warn("destroying orphaned runner: dispatched but never assigned a job within the grace window",
			"runner", v.name,
			"dispatched_for_job", v.key.JobID,
			"grace", grace)
		metrics.JobsActive.Dec()
		v.rj.cancel()
		if v.rj.macosVM != nil {
			v.rj.macosVM.Stop()
		} else if v.rj.nativeRunner != nil {
			v.rj.nativeRunner.Stop()
		} else if v.rj.dispatched != "" && s.cfg.LinuxDispatcher != nil {
			if err := s.cfg.LinuxDispatcher.Destroy(context.Background(), v.rj.dispatched); err != nil {
				s.cfg.Log.Warn("failed to destroy orphaned dispatched runner", "runner", v.name, "error", err)
			}
		} else if v.rj.env != nil {
			if err := s.cfg.Runtime.Destroy(context.Background(), v.rj.env); err != nil {
				s.cfg.Log.Warn("failed to destroy orphaned runner environment", "runner", v.name, "error", err)
			}
		}
		if v.rj.artifactsDir != "" {
			artifacts.Cleanup(v.rj.artifactsDir, s.cfg.Log)
		}
		// Deregister the JIT runner from the provider: it never ran a
		// job, so it will not auto-remove itself and would otherwise
		// linger as an offline ghost.
		if v.rj.provider != nil && v.rj.claim != nil {
			if err := v.rj.provider.ReleaseJob(context.Background(), v.rj.claim); err != nil {
				s.cfg.Log.Debug("deregister orphaned runner", "runner", v.name, "error", err)
			}
		}
	}
}

// enqueueRetryIfEligible passes err through the retry queue, if enabled.
// The retryHandler callback re-invokes handleQueued with the ORIGINAL
// event when the backoff timer fires. Non-retryable errors and disabled
// queues are a no-op. Safe to call with s.retry == nil.
func (s *Scheduler) enqueueRetryIfEligible(ctx context.Context, event providers.JobEvent, err error) {
	// On the retry path, retryHandler put a *error in the context: hand the
	// claim error back to it (preserving the error class) instead of
	// enqueuing. Enqueuing here as well would be undone by runOne's
	// success-cleanup and lose the job.
	if errPtr, ok := ctx.Value(retryAttemptCtxKey{}).(*error); ok && errPtr != nil {
		*errPtr = err
		return
	}
	if s.retry == nil {
		return
	}
	s.retry.Add(event, s.retryHandler, err)
}

// retryHandler is the callback the retry queue invokes on each fire.
// It re-enters the top-level dispatch (handleQueued) with the ORIGINAL
// event. Because handleQueued would otherwise dedup our own retry via
// the seen/pending maps, we clear those entries first.
//
// Return value: always nil. handleQueued dispatches asynchronously into
// concurrency slots so we cannot know synchronously whether the retry
// succeeded; on failure the handler self-enqueues via
// enqueueRetryIfEligible, and on success any future completed webhook
// harmlessly Drops the (already-absent) key.
type retryAttemptCtxKey struct{}

func (s *Scheduler) retryHandler(ctx context.Context, event providers.JobEvent) error {
	key := keyFor(event)
	s.mu.Lock()
	// Clear seen/pending so handleQueued does not dedup our own retry.
	// Leave running alone: if the job got picked up elsewhere, we
	// want handleQueued's running-check to short-circuit.
	delete(s.seen, key)
	delete(s.pending, key)
	s.mu.Unlock()
	// Re-dispatch. On a claim failure enqueueRetryIfEligible writes the
	// error into claimErr (and suppresses a duplicate enqueue), so we can
	// return it: nil => claimed/dispatched OK (runOne drops the retry);
	// non-nil => still failing (runOne advances the ladder with the real
	// error class preserved).
	var claimErr error
	rctx := context.WithValue(ctx, retryAttemptCtxKey{}, &claimErr)
	s.handleQueued(rctx, event)
	return claimErr
}

// buildLabelsForOS builds runner labels for a given target OS.
// Used by the dispatcher to register Linux runners from the Windows host.
func buildLabelsForOS(targetOS string, extraLabels []string) []string {
	labels := []string{"self-hosted"}

	switch targetOS {
	case "windows":
		labels = append(labels, "windows")
	case "darwin":
		labels = append(labels, "macos")
	default:
		labels = append(labels, "linux")
	}

	switch goruntime.GOARCH {
	case "arm64":
		labels = append(labels, "arm64")
	default:
		labels = append(labels, "x64")
	}

	labels = append(labels, extraLabels...)

	return labels
}

// claimJob generates a runner name and claims the job via the Provider,
// retrying with a new name if the name already exists (409 conflict).
func (s *Scheduler) claimJob(ctx context.Context, event *providers.JobEvent, labels []string, log *slog.Logger, maxRetries int) (*providers.Claim, error) {
	var lastErr error
	for attempt := range maxRetries {
		name := fmt.Sprintf("ephemerd-%s-%s-%s", event.Provider.Name(), event.Repo, names.Generate())
		claim, err := event.Provider.ClaimJob(ctx, event, name, labels)
		if err == nil {
			return claim, nil
		}
		lastErr = err
		if isConflict(err) && attempt < maxRetries-1 {
			log.Warn("runner name conflict, retrying with new name", "name", name, "attempt", attempt+1)
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

// isConflict reports whether an error is a GitHub 409 Conflict (runner name already exists).
func isConflict(err error) bool {
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) {
		return ghErr.Response.StatusCode == http.StatusConflict
	}
	// The error may be wrapped in a way errors.As can't unwrap — fall back to string match.
	return strings.Contains(err.Error(), "409")
}

const (
	tunnelReconnectDelay    = 5 * time.Second
	tunnelMaxReconnectDelay = 60 * time.Second
	defaultTunnelMaxRetries = 5
)

// serveTunnelWithReconnect serves the webhook HTTP server on a tunnel listener,
// automatically re-establishing the tunnel and re-registering webhooks when the
// connection drops. Falls back to polling after maxRetries consecutive failures.
//
// Each reconnect cycle creates a fresh http.Server because Go's http.Server
// cannot be reused after Serve() returns — its internal state (shutdown flag,
// connection tracking) is not reset. The handler mux is shared across all
// server instances since it's stateless.
func (s *Scheduler) serveTunnelWithReconnect(ctx context.Context, handler http.Handler, ln net.Listener, whProviders []providers.Webhook, events chan<- providers.JobEvent) {
	maxRetries := s.cfg.TunnelMaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultTunnelMaxRetries
	}

	// On exit, clean up whichever webhooks are currently active.
	defer func() {
		for _, whp := range whProviders {
			if err := whp.DeregisterWebhooks(context.Background()); err != nil {
				s.cfg.Log.Warn("failed to deregister webhooks on shutdown",
					"provider", whp.(providers.Provider).Name(), "error", err)
			}
		}
	}()

	consecutiveFailures := 0
	delay := tunnelReconnectDelay

	for {
		// Create a fresh server for each tunnel listener. http.Server
		// cannot be reused after Serve() returns.
		server := &http.Server{Handler: handler}

		// Watch for context cancellation so we can unblock Serve().
		// http.Server.Serve blocks on the listener and doesn't check
		// ctx.Done — we need to shut down the server explicitly.
		go func() {
			<-ctx.Done()
			_ = server.Close()
		}()

		err := server.Serve(ln)

		if ctx.Err() != nil {
			// Parent context cancelled — clean shutdown.
			return
		}

		// Shut down the server to release its internal state before
		// we create a new one. (The ctx watcher goroutine above may
		// also call Close, which is safe to call multiple times.)
		_ = server.Close()
		consecutiveFailures++
		s.cfg.Log.Warn("tunnel connection lost, reconnecting",
			"error", err,
			"failure", consecutiveFailures,
			"max_retries", maxRetries,
		)

		// Close the dead listener to stop its goroutines (localtunnel
		// proxy workers, ngrok tunnel connection). Without this, each
		// reconnect leaks the old listener's resources.
		_ = ln.Close()

		if consecutiveFailures >= maxRetries {
			s.cfg.Log.Warn("tunnel max retries exceeded, falling back to polling",
				"failures", consecutiveFailures,
			)
			// Best-effort cleanup of all webhook providers.
			for _, whp := range whProviders {
				if err := whp.DeregisterWebhooks(ctx); err != nil {
					s.cfg.Log.Warn("failed to deregister webhooks on tunnel fallback",
						"provider", whp.(providers.Provider).Name(), "error", err)
				}
			}

			// Fall back to polling for all poll-capable providers.
			interval := s.cfg.PollInterval
			if interval <= 0 {
				interval = 10 * time.Second
			}
			for _, p := range s.cfg.Providers {
				pollProvider, ok := p.(providers.Poll)
				if !ok {
					continue
				}
				s.cfg.Log.Info("polling mode enabled (tunnel fallback)", "provider", p.Name(), "interval", interval)
				pollCh, err := pollProvider.Start(ctx, providers.PollConfig{
					PollInterval: int(interval.Seconds()),
				})
				if err != nil {
					s.cfg.Log.Error("failed to start poll fallback", "provider", p.Name(), "error", err)
					continue
				}
				go func(ch <-chan providers.JobEvent) {
					for ev := range ch {
						events <- ev
					}
				}(pollCh)
			}
			return
		}

		// Deregister old webhooks (best-effort — URL is dead anyway).
		for _, whp := range whProviders {
			if err := whp.DeregisterWebhooks(ctx); err != nil {
				s.cfg.Log.Debug("failed to deregister old webhooks", "error", err)
			}
		}

		// Exponential backoff reconnect.
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		newLn, err := s.cfg.Tunnel.Listen(ctx)
		if err != nil {
			s.cfg.Log.Warn("tunnel reconnect failed", "error", err, "next_delay", delay)
			delay = min(delay*2, tunnelMaxReconnectDelay)
			continue
		}

		// Tunnel is back — re-register webhooks with the new URL for all providers.
		allOK := true
		for _, whp := range whProviders {
			webhookURL := s.cfg.Tunnel.PublicURL() + "/webhook/" + whp.(providers.Provider).Name()
			if err := whp.RegisterWebhooks(ctx, webhookURL, s.cfg.WebhookSecret); err != nil {
				s.cfg.Log.Error("failed to re-register webhooks after tunnel reconnect",
					"provider", whp.(providers.Provider).Name(), "error", err)
				allOK = false
			}
		}
		if !allOK {
			_ = newLn.Close()
			delay = min(delay*2, tunnelMaxReconnectDelay)
			continue
		}

		s.cfg.Log.Info("tunnel reconnected", "url", s.cfg.Tunnel.PublicURL())
		ln = newLn
		consecutiveFailures = 0
		delay = tunnelReconnectDelay
	}
}
