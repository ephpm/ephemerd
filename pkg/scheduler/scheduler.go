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
	"github.com/ephpm/ephemerd/pkg/providers"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"github.com/ephpm/ephemerd/pkg/tunnel"
	"github.com/ephpm/ephemerd/pkg/vm"
	gh "github.com/google/go-github/v72/github"
)

// Config for the scheduler.
type Config struct {
	Runtime         *runtime.Runtime
	Providers       []providers.Provider
	Artifacts       *artifacts.Extractor  // OCI image layer extractor for macOS VM jobs (nil if not available)
	LinuxDispatcher *DispatchClient       // if non-nil, Linux jobs are dispatched to a Linux VM worker via gRPC
	MacOSVMConfig     *vm.MacOSVMConfig     // if non-nil, macOS-native jobs are enabled (darwin only)
	DataDir           string                // ephemerd data directory (used for artifact extraction paths)
	MaxConcurrent     int
	MaxMacOSVMs       int                   // max concurrent macOS VMs (Vz limit; default auto-detected)
	Labels          []string
	PollInterval    time.Duration // if >0, use polling mode (default)
	WebhookPort     int           // listen port for health/webhook server
	WebhookSecret   string        // webhook signature secret
	TLSCert         string        // TLS certificate path
	TLSKey          string        // TLS private key path
	Tunnel            tunnel.Provider // if non-nil, creates a public tunnel for webhooks
	TunnelMaxRetries  int             // max consecutive reconnect failures before fallback to polling (0 = default 5)
	JobTimeout        time.Duration
	ShutdownTimeout time.Duration
	LogRetention    time.Duration // max age for job log files (default 7d)

	// RunnerImageForRepo resolves the per-repo, per-OS image override
	// configured under [runner.images]. Returns "" when no override is
	// set; the scheduler then falls back to the provider per-OS default
	// and finally the runtime's host-aware default. Nil-safe.
	RunnerImageForRepo func(repo, os string) string

	Log *slog.Logger
}

// resolveImage returns the runner image to launch for an event.
//
// Resolution order:
//
//	1. Image declared in the workflow YAML (FetchJobImage)
//	2. Per-repo override from [runner.images.<repo>].<os>
//	3. Provider per-OS default (DefaultImageFor)
//	4. Empty — runtime.Create picks its host-aware fallback
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
	cfg       Config
	running   map[jobKey]*runningJob
	seen      map[jobKey]time.Time // recently handled jobs for dedup
	mu        sync.Mutex
	sem       chan struct{} // concurrency limiter
	macSem    chan struct{} // macOS VM concurrency limiter (Vz has a hard cap)
	draining  bool         // true when shutting down, rejects new jobs
	startTime time.Time
}

const seenTTL = 10 * time.Minute

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
	artifactsDir string    // non-empty if OCI artifacts were extracted for this job
	dispatched   string    // non-empty if dispatched to Linux VM worker (stores container name)
	macosVM      vm.MacOSVM // non-nil if running as a macOS VM job
	startedAt    time.Time
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

	return &Scheduler{
		cfg:       cfg,
		running:   make(map[jobKey]*runningJob),
		seen:      make(map[jobKey]time.Time),
		sem:       make(chan struct{}, cfg.MaxConcurrent),
		macSem:    make(chan struct{}, macVMs),
		startTime: time.Now(),
	}
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

	// Periodically clean up the seen-jobs dedup map
	cleanupTicker := time.NewTicker(5 * time.Minute)
	defer cleanupTicker.Stop()

	// Process events from all providers via the unified events channel.
	for {
		select {
		case <-cleanupTicker.C:
			s.cleanSeen()

		case <-ctx.Done():
			s.cfg.Log.Info("shutting down scheduler")
			s.drain()
			return nil

		case event := <-events:
			switch event.Action {
			case "queued":
				metrics.JobsQueuedTotal.Inc()
				go s.handleQueued(ctx, event)
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
			// macOS jobs need a per-job VM for isolation. Without
			// MacOSVMConfig we refuse the job rather than fall back to
			// running on the host — sharing the runner process tree with
			// other jobs (and the daemon) is a non-starter for CI.
			osOK = goruntime.GOOS == "darwin" && s.cfg.MacOSVMConfig != nil
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
	if t, seen := s.seen[key]; seen && time.Since(t) < seenTTL {
		s.mu.Unlock()
		log.Debug("ignoring duplicate queued event, job recently handled")
		return
	}
	s.seen[key] = time.Now()

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

	// Route macOS-native jobs to per-job macOS VMs.
	if isMacOSJob(event.Labels) {
		s.mu.Lock()
		macCfg := s.cfg.MacOSVMConfig
		s.mu.Unlock()
		if macCfg != nil {
			s.handleMacOSJob(ctx, event)
			return
		}
		// macOS VM disk is still being provisioned — remove from seen so
		// the next poll retries this job once the install finishes.
		s.mu.Lock()
		delete(s.seen, key)
		s.mu.Unlock()
		log.Info("macOS VM disk not ready yet, deferring job")
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
		s.mu.Unlock()
	}

	// Acquire concurrency slot
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		unsee()
		return
	}

	log.Info("provisioning Linux runner via dispatch")

	image := s.resolveImage(ctx, &event, "linux")
	if image != "" {
		log.Info("using image for job", "image", image, "repo", event.Repo)
	}

	labels := buildLabelsForOS("linux", s.cfg.Labels)

	const maxNameRetries = 3
	claim, err := s.claimJob(ctx, &event, labels, log, maxNameRetries)
	if err != nil {
		log.Error("failed to claim job", "error", err)
		unsee()
		time.Sleep(backoffDuration(event.Repo))
		<-s.sem
		return
	}

	var jobCtx context.Context
	var cancel context.CancelFunc
	if s.cfg.JobTimeout > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, s.cfg.JobTimeout)
	} else {
		jobCtx, cancel = context.WithCancel(ctx)
	}

	if err := s.cfg.LinuxDispatcher.Create(jobCtx, claim.RunnerName, image, claim.RunnerConfig); err != nil {
		log.Error("dispatch create failed", "error", err)
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		unsee()
		cancel()
		<-s.sem
		return
	}

	// Track the dispatched job (env is nil — lifecycle managed by Linux VM worker)
	s.mu.Lock()
	s.running[key] = &runningJob{
		provider:   event.Provider,
		claim:      claim,
		repo:       event.Repo,
		image:      image,
		cancel:     cancel,
		dispatched: claim.RunnerName,
		startedAt:  time.Now(),
	}
	s.mu.Unlock()
	metrics.JobsActive.Inc()

	log.Info("Linux runner dispatched", "name", claim.RunnerName)

	// Wait for the job to finish in the background
	go func() {
		defer func() { <-s.sem }()

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

		// Always clean up
		s.mu.Lock()
		_, exists := s.running[key]
		if exists {
			delete(s.running, key)
		}
		s.mu.Unlock()

		if err := s.cfg.LinuxDispatcher.Destroy(context.Background(), claim.RunnerName); err != nil {
			log.Warn("dispatch destroy failed", "error", err)
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
		s.mu.Unlock()
	}

	// Acquire concurrency slots: general + macOS VM limit.
	// Vz caps the number of simultaneous VMs based on host resources.
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		unsee()
		return
	}
	select {
	case s.macSem <- struct{}{}:
	case <-ctx.Done():
		<-s.sem
		unsee()
		return
	}

	log.Info("provisioning macOS VM runner for job")

	// Extract OCI artifacts if an image is specified
	image := event.Provider.FetchJobImage(ctx, &event)
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
		log.Error("failed to claim job", "error", err)
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		unsee()
		time.Sleep(backoffDuration(event.Repo))
		<-s.macSem
		<-s.sem
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
		<-s.sem
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
		<-s.sem
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
		<-s.sem
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
		<-s.sem
		return
	}

	// Track the running job
	s.mu.Lock()
	s.running[key] = &runningJob{
		provider:   event.Provider,
		claim:        claim,
		repo:         event.Repo,
		image:        image,
		cancel:       cancel,
		artifactsDir: artifactsDir,
		macosVM:      macVM,
		startedAt:    time.Now(),
	}
	s.mu.Unlock()
	metrics.JobsActive.Inc()

	log.Info("macOS VM runner ready", "name", claim.RunnerName, "ip", ip)

	// Wait for the job to finish in the background
	go func() {
		defer func() { <-s.macSem; <-s.sem }()

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
			delete(s.running, key)
			s.mu.Unlock()
			rj.macosVM.Stop()
			if rj.artifactsDir != "" {
				artifacts.Cleanup(rj.artifactsDir, s.cfg.Log)
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

	// On provisioning failure, remove from seen so the next poll retries
	unsee := func() {
		s.mu.Lock()
		delete(s.seen, key)
		s.mu.Unlock()
	}

	// Acquire concurrency slot
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		unsee()
		return
	}

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
		log.Error("failed to claim job", "error", err)
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		unsee()
		time.Sleep(5 * time.Second) // back off to avoid tight retry loops on rate limits
		<-s.sem
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
	s.mu.Lock()
	s.running[key] = &runningJob{
		env:          env,
		claim:        claim,
		repo:         event.Repo,
		image:        image,
		cancel:       cancel,
		artifactsDir: artifactsDir,
		startedAt:    time.Now(),
	}
	s.mu.Unlock()
	metrics.JobsActive.Inc()

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
			delete(s.running, key)
			s.mu.Unlock()
			if err := s.cfg.Runtime.Destroy(context.Background(), env); err != nil {
				log.Warn("failed to destroy runner environment", "error", err)
			}
			if rj.artifactsDir != "" {
				artifacts.Cleanup(rj.artifactsDir, s.cfg.Log)
			}
		} else {
			s.mu.Unlock()
		}
	}()
}

func (s *Scheduler) handleCompleted(ctx context.Context, event providers.JobEvent) {
	jobID := event.JobID
	key := keyFor(event)
	log := s.cfg.Log.With("job_id", jobID, "repo", event.Repo)

	s.mu.Lock()
	job, exists := s.running[key]
	if exists {
		delete(s.running, key)
	}
	s.mu.Unlock()

	if !exists {
		return
	}

	conclusion := event.Conclusion
	log.Info("job completed, destroying runner environment",
		"conclusion", conclusion,
	)

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
	s.mu.Unlock()

	for key, job := range jobs {
		s.cfg.Log.Info("destroying runner on shutdown", "job_id", key.JobID, "provider", key.Provider)
		job.cancel()
		if job.macosVM != nil {
			job.macosVM.Stop()
		} else if job.dispatched != "" && s.cfg.LinuxDispatcher != nil {
			if err := s.cfg.LinuxDispatcher.Destroy(context.Background(), job.dispatched); err != nil {
				s.cfg.Log.Warn("failed to destroy dispatched runner", "job_id", key.JobID, "error", err)
			}
		} else if job.env != nil {
			if err := s.cfg.Runtime.Destroy(context.Background(), job.env); err != nil {
				s.cfg.Log.Warn("failed to destroy runner environment", "job_id", key.JobID, "error", err)
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
		}
	}
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
