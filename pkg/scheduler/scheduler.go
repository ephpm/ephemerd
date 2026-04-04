package scheduler

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	goruntime "runtime"
	"sync"

	"github.com/ephpm/ephemerd/pkg/github"
	"github.com/ephpm/ephemerd/pkg/runtime"
)

// Config for the scheduler.
type Config struct {
	Runtime       *runtime.Runtime
	GitHub        *github.Client
	MaxConcurrent int
	Labels        []string
	WebhookPort   int
	WebhookSecret string
	Log           *slog.Logger
}

// Scheduler ties GitHub job events to container lifecycle.
// When a workflow_job is queued, it provisions a runner environment.
// When the job completes, it destroys the environment.
type Scheduler struct {
	cfg     Config
	running map[int64]*runningJob
	mu      sync.Mutex
	sem     chan struct{} // concurrency limiter
}

type runningJob struct {
	env    *runtime.RunnerEnv
	repo   string
	cancel context.CancelFunc
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
		cfg:     cfg,
		running: make(map[int64]*runningJob),
		sem:     make(chan struct{}, cfg.MaxConcurrent),
	}
}

// Run starts the scheduler. It listens for GitHub webhook events
// and manages runner environment lifecycle.
func (s *Scheduler) Run(ctx context.Context) error {
	handler, events := s.cfg.GitHub.WebhookHandler(s.cfg.WebhookSecret)

	// Start webhook server
	mux := http.NewServeMux()
	mux.Handle("/webhook", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

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

	// Process events
	for {
		select {
		case <-ctx.Done():
			s.cfg.Log.Info("shutting down scheduler")
			s.destroyAll()
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
		<-s.sem
		return
	}

	// Encode the JIT config for the runner
	encodedConfig := base64.StdEncoding.EncodeToString([]byte(jitConfig.GetEncodedJITConfig()))

	// Create the runner environment
	jobCtx, cancel := context.WithCancel(ctx)
	env, err := s.cfg.Runtime.Create(jobCtx, name, "", encodedConfig)
	if err != nil {
		log.Error("failed to create runner environment", "error", err)
		cancel()
		<-s.sem
		return
	}

	// Track the running job
	s.mu.Lock()
	s.running[jobID] = &runningJob{
		env:    env,
		repo:   event.Repo,
		cancel: cancel,
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
