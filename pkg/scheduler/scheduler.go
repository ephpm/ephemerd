package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/github"
	"github.com/ephpm/ephemerd/pkg/runtime"
)

// Config for the scheduler.
type Config struct {
	Runtime         *runtime.Runtime
	GitHub          *github.Client
	MaxConcurrent   int
	Labels          []string
	WebhookPort     int
	WebhookSecret   string
	JobTimeout      time.Duration
	ShutdownTimeout time.Duration
	Log             *slog.Logger
}

// Scheduler ties GitHub job events to container lifecycle.
// When a workflow_job is queued, it provisions a runner environment.
// When the job completes, it destroys the environment.
type Scheduler struct {
	cfg       Config
	running   map[int64]*runningJob
	seen      map[int64]time.Time // recently handled job IDs for dedup
	mu        sync.Mutex
	sem       chan struct{} // concurrency limiter
	draining  bool         // true when shutting down, rejects new jobs
	startTime time.Time
}

const seenTTL = 10 * time.Minute

type runningJob struct {
	env      *runtime.RunnerEnv
	repo     string
	runnerID int64
	cancel   context.CancelFunc
}

// New creates a scheduler.
func New(cfg Config) *Scheduler {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.WebhookPort <= 0 {
		cfg.WebhookPort = 8080
	}

	return &Scheduler{
		cfg:       cfg,
		running:   make(map[int64]*runningJob),
		seen:      make(map[int64]time.Time),
		sem:       make(chan struct{}, cfg.MaxConcurrent),
		startTime: time.Now(),
	}
}

// Run starts the scheduler. It listens for GitHub webhook events
// and manages runner environment lifecycle.
func (s *Scheduler) Run(ctx context.Context) error {
	handler, events := s.cfg.GitHub.WebhookHandler(s.cfg.WebhookSecret)

	// Start webhook server
	mux := http.NewServeMux()
	mux.Handle("/webhook", handler)
	mux.HandleFunc("/healthz", s.handleHealthz)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.cfg.WebhookPort),
		Handler: mux,
	}

	// Start HTTP server in background
	go func() {
		s.cfg.Log.Info("webhook server listening", "port", s.cfg.WebhookPort)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			s.cfg.Log.Error("webhook server error", "error", err)
		}
	}()

	// Periodically clean up the seen-jobs dedup map
	cleanupTicker := time.NewTicker(5 * time.Minute)
	defer cleanupTicker.Stop()

	// Process events
	for {
		select {
		case <-cleanupTicker.C:
			s.cleanSeen()

		case <-ctx.Done():
			s.cfg.Log.Info("shutting down scheduler")
			s.drain()
			server.Shutdown(context.Background())
			return nil

		case event := <-events:
			switch event.Action {
			case "queued":
				go s.handleQueued(ctx, event)
			case "completed":
				go s.handleCompleted(ctx, event)
			}
		}
	}
}

func (s *Scheduler) handleQueued(ctx context.Context, event github.JobEvent) {
	jobID := event.Job.GetID()
	log := s.cfg.Log.With("job_id", jobID, "repo", event.Repo)

	// Dedup: skip if we've already seen this job recently
	s.mu.Lock()
	if _, exists := s.running[jobID]; exists {
		s.mu.Unlock()
		log.Debug("ignoring duplicate queued event, job already running")
		return
	}
	if t, seen := s.seen[jobID]; seen && time.Since(t) < seenTTL {
		s.mu.Unlock()
		log.Debug("ignoring duplicate queued event, job recently handled")
		return
	}
	s.seen[jobID] = time.Now()

	if s.draining {
		s.mu.Unlock()
		log.Info("rejecting job, scheduler is draining")
		return
	}
	s.mu.Unlock()

	// Acquire concurrency slot
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}

	log.Info("provisioning runner for job")

	// Build runner labels
	labels := s.buildLabels()

	// Generate a unique runner name
	name := fmt.Sprintf("ephemerd-%s-%d", event.Repo, jobID)

	// Register a JIT runner with GitHub
	jitConfig, err := s.cfg.GitHub.RegisterJITRunner(ctx, event.Repo, name, labels)
	if err != nil {
		log.Error("failed to register JIT runner", "error", err)
		time.Sleep(5 * time.Second) // back off to avoid tight retry loops on rate limits
		<-s.sem
		return
	}

	// The GitHub API returns the JIT config already base64-encoded;
	// the runner binary expects it as-is.
	encodedConfig := jitConfig.GetEncodedJITConfig()

	// Create the runner environment with job timeout
	runnerID := jitConfig.GetRunner().GetID()
	var jobCtx context.Context
	var cancel context.CancelFunc
	if s.cfg.JobTimeout > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, s.cfg.JobTimeout)
	} else {
		jobCtx, cancel = context.WithCancel(ctx)
	}
	env, err := s.cfg.Runtime.Create(jobCtx, name, "", encodedConfig)
	if err != nil {
		log.Error("failed to create runner environment", "error", err)
		// Remove the ghost runner from GitHub since the container won't start
		if rmErr := s.cfg.GitHub.RemoveRunner(ctx, event.Repo, runnerID); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", runnerID, "error", rmErr)
		}
		cancel()
		<-s.sem
		return
	}

	// Track the running job
	s.mu.Lock()
	s.running[jobID] = &runningJob{
		env:      env,
		repo:     event.Repo,
		runnerID: runnerID,
		cancel:   cancel,
	}
	s.mu.Unlock()

	log.Info("runner environment ready", "name", name)

	// Wait for the job to finish in the background
	go func() {
		defer func() {
			<-s.sem // release concurrency slot
		}()

		exitCode, err := s.cfg.Runtime.Wait(jobCtx, env)
		if err != nil {
			log.Warn("runner exited with error", "error", err)
		} else {
			log.Info("runner exited", "exit_code", exitCode)
		}

		// Clean up if not already handled by completed event
		s.mu.Lock()
		if _, exists := s.running[jobID]; exists {
			delete(s.running, jobID)
			s.mu.Unlock()
			s.cfg.Runtime.Destroy(context.Background(), env)
		} else {
			s.mu.Unlock()
		}
	}()
}

func (s *Scheduler) handleCompleted(ctx context.Context, event github.JobEvent) {
	jobID := event.Job.GetID()
	log := s.cfg.Log.With("job_id", jobID, "repo", event.Repo)

	s.mu.Lock()
	job, exists := s.running[jobID]
	if exists {
		delete(s.running, jobID)
	}
	s.mu.Unlock()

	if !exists {
		return
	}

	log.Info("job completed, destroying runner environment",
		"conclusion", event.Job.GetConclusion(),
	)

	job.cancel()
	s.cfg.Runtime.Destroy(context.Background(), job.env)
}

// drain stops accepting new jobs and waits for running jobs to finish.
// If jobs don't finish within ShutdownTimeout, they are force-killed.
func (s *Scheduler) drain() {
	s.mu.Lock()
	s.draining = true
	count := len(s.running)
	s.mu.Unlock()

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
	jobs := make(map[int64]*runningJob, len(s.running))
	for k, v := range s.running {
		jobs[k] = v
	}
	s.running = make(map[int64]*runningJob)
	s.mu.Unlock()

	for id, job := range jobs {
		s.cfg.Log.Info("destroying runner on shutdown", "job_id", id)
		job.cancel()
		s.cfg.Runtime.Destroy(context.Background(), job.env)
	}
}

func (s *Scheduler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	activeJobs := len(s.running)
	draining := s.draining
	s.mu.Unlock()

	status := map[string]any{
		"status":        "ok",
		"active_jobs":   activeJobs,
		"max_concurrent": s.cfg.MaxConcurrent,
		"draining":      draining,
		"uptime":        time.Since(s.startTime).String(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
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

func (s *Scheduler) buildLabels() []string {
	labels := []string{"self-hosted"}

	// Add OS label
	switch goruntime.GOOS {
	case "windows":
		labels = append(labels, "windows")
	default:
		labels = append(labels, "linux")
	}

	// Add arch label
	switch goruntime.GOARCH {
	case "arm64":
		labels = append(labels, "arm64")
	default:
		labels = append(labels, "x64")
	}

	labels = append(labels, s.cfg.Labels...)

	return labels
}
