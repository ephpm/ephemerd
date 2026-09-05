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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/artifacts"
	"github.com/ephpm/ephemerd/pkg/cacheprune"
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
	Artifacts       *artifacts.Extractor // OCI image layer extractor for macOS VM jobs (nil if not available)
	LinuxDispatcher *DispatchClient      // if non-nil, Linux jobs are dispatched to a Linux VM worker via gRPC
	// LinuxJobsDisabled makes canHandleJob refuse Linux-labeled jobs even on
	// a host that could run them ([vm.linux] enabled = false on darwin). Used
	// when a dedicated Linux box serves the same labels: without it, both
	// daemons register a JIT runner per queued job and the loser's runner
	// squats as an orphan until the grace sweep.
	LinuxJobsDisabled bool
	MacOSVMConfig     *vm.MacOSVMConfig // if non-nil, macOS-native jobs are enabled (darwin only)
	// CachePruner reclaims disk held by daemon-managed caches (BuildKit's
	// build cache, the containerd image store) on demand, through the
	// manager that owns each one. Backs the PruneCache RPC that
	// `ephemerd cache clear` uses so an operator no longer has to stop the
	// daemon and delete directories. Nil makes PruneCache return
	// Unimplemented.
	CachePruner       cacheprune.Interface
	DataDir           string // ephemerd data directory (used for artifact extraction paths)
	Version           string // daemon build version (from main.version); surfaced via Status and used by the Upgrade RPC
	MaxConcurrent     int
	MaxMacOSVMs       int // max concurrent macOS VMs (Vz limit; default auto-detected)
	Labels            []string
	PollInterval      time.Duration   // if >0, use polling mode (default)
	ReconcileInterval time.Duration   // webhook mode: periodic catch-up sweep for stranded jobs (0 = disabled)
	WebhookPort       int             // listen port for health/webhook server
	WebhookSecret     string          // webhook signature secret
	TLSCert           string          // TLS certificate path
	TLSKey            string          // TLS private key path
	Tunnel            tunnel.Provider // if non-nil, creates a public tunnel for webhooks
	TunnelMaxRetries  int             // max consecutive reconnect failures before fallback to polling (0 = default 5)

	// ExternalURL is the public base URL of an externally-managed tunnel
	// (tunnel = "external"). When set alongside webhook mode and NO managed
	// Tunnel, the scheduler registers each webhook-capable provider's hook to
	// <ExternalURL>/webhook/<provider> on startup, so the operator doesn't
	// have to hand-add a hook per repo. External hooks are operator-owned and
	// are NOT deregistered on shutdown. Empty means "receiver only, don't
	// touch the platform's webhooks".
	ExternalURL     string
	JobTimeout      time.Duration
	ShutdownTimeout time.Duration
	LogRetention    time.Duration // max age for job log files (default 7d)

	// MacOSProvisionTimeout bounds the pre-registration provisioning phase of
	// a macOS VM job — booting the VM and waiting for its runner to become
	// reachable (handleMacOSJob → MacOSVM.WaitForRunner). If the wait has not
	// returned by this deadline the VM is force-stopped so the reachability
	// wait unblocks, the job is failed, and the single macOS concurrency slot
	// is released. Guards against a hung guest SSH command wedging the wait
	// indefinitely (the VM-internal loop caps itself at ~2 min, but its SSH
	// session calls carry no deadline and ignore ctx). Zero applies
	// defaultMacOSProvisionTimeout.
	MacOSProvisionTimeout time.Duration

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
		// A non-empty FetchJobImage means the job declared `container:` in
		// its workflow. ephemerd honors that by making it the RUNNER image,
		// which is the whole of the support on Windows — the Actions runner
		// then goes on to do its own container setup for the same directive
		// (docker pull/create/exec), and neither half of that works there:
		// the stock Windows images carry no docker CLI, and pkg/dind cannot
		// create Windows sibling containers at all (checkWindowsSiblingGate).
		// The job fails with "docker: command not found" in Set up job, which
		// says nothing about why — so say it here, where we know. On a stock
		// Windows image this log line is the ONLY signal: the job never gets
		// far enough to reach dind's gate.
		//
		// Reached for `container:` only. A job that declares `services:` has
		// the same problem and gets no warning, because parseContainerImage
		// (pkg/github/client.go) reads the `container` key and nothing else.
		// Widening it means parsing `services:` purely to warn about it.
		if os == "windows" && s.cfg.Log != nil {
			s.cfg.Log.Warn("job declares container: on a Windows runner — expect it to fail in Set up job",
				"image", img, "repo", event.Repo, "job_id", event.JobID,
				"reason", "ephemerd runs the job inside this image, but the Actions runner also "+
					"performs its own container setup, which is not supported on Windows",
				"workaround", "drop container: from the Windows leg and select the image with [runner.images.<repo>].windows")
		}
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

// defaultMacOSProvisionTimeout bounds a macOS VM job's provisioning phase
// (boot + wait-for-runner-reachable) when Config.MacOSProvisionTimeout is
// zero. Generous relative to a healthy provision (~1 min) so it never trips a
// slow-but-progressing boot, but small enough that a wedged VM frees the sole
// macOS slot in minutes instead of hours.
const defaultMacOSProvisionTimeout = 5 * time.Minute

// macProvisionUnwindGrace bounds how long the provisioning watchdog waits for
// a force-stopped macOS VM to tear down and its reachability wait to unwind
// before giving up on both and returning anyway.
//
// Comfortably more than darwinMacOSVM.stop's own 15s graceful-shutdown window
// plus the force-stop that follows it, so a VM that CAN be killed is always
// reaped tidily. Past that we stop caring: the whole point of the deadline was
// to get the concurrency slot back, and waiting on an unkillable VM to say so
// is what cost the fleet 28 hours of macOS CI (#196).
const macProvisionUnwindGrace = 30 * time.Second

// macTeardownGrace bounds a routine macOS VM Stop() on any path that is
// holding a slot or a wait-goroutine. Same reasoning as
// macProvisionUnwindGrace: teardown is best-effort, capacity is not.
const macTeardownGrace = 30 * time.Second

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

// labelSetKey returns a canonical identity for a job's label set — the
// FUNGIBILITY CLASS the job belongs to.
//
// GitHub does not honor the pairing ephemerd creates when it registers a JIT
// runner "for" a job: it hands a connected runner ANY queued job whose labels
// the runner satisfies. So the unit that dispatch accounting must balance is
// not the job id, it is the label set: N queued jobs sharing a label set are
// served by N runners of that class, in whatever permutation GitHub picks.
//
// Labels are lowercased (GitHub matches case-insensitively), trimmed, emptied
// entries dropped, sorted and deduped so that ["Linux","self-hosted"] and
// ["self-hosted","linux","linux"] land in the same class. The separator is a
// unit separator so it cannot occur inside a label.
func labelSetKey(labels []string) string {
	norm := make([]string, 0, len(labels))
	for _, l := range labels {
		l = strings.ToLower(strings.TrimSpace(l))
		if l != "" {
			norm = append(norm, l)
		}
	}
	sort.Strings(norm)
	uniq := make([]string, 0, len(norm))
	for i, l := range norm {
		if i == 0 || l != norm[i-1] {
			uniq = append(uniq, l)
		}
	}
	return strings.Join(uniq, "\x1f")
}

// Scheduler ties CI provider job events to container lifecycle.
// When a job is queued, it provisions a runner environment.
// When the job completes, it destroys the environment.
type Scheduler struct {
	cfg         Config
	running     map[jobKey]*runningJob
	seen        map[jobKey]time.Time      // recently handled jobs for dedup
	started     map[jobKey]time.Time      // jobs OBSERVED going in_progress/completed (webhook mode); the event-driven "satisfied" signal
	pending     map[jobKey]struct{}       // jobs dispatched to a handler but not yet holding sem
	attempts    map[jobKey]int            // provisioning passes per job, for zombie detection
	jobLabels   map[jobKey]string         // fungibility class (labelSetKey) of every accepted job; pruned with seen
	runners     map[string]*runnerBinding // dispatched runners by name; tracks observed job assignment
	webhookMode bool                      // true when job events arrive via webhooks (in_progress observable)
	runCtx      context.Context           // scheduler root context (set once at Run start); used for event-driven re-dispatch
	jobsCtx     context.Context           // detached parent for job runtimes; survives runCtx (signal) cancellation
	jobsCancel  context.CancelFunc        // cancels jobsCtx; called by drain() once the wait/force-kill phase ends
	mu          sync.Mutex
	sem         chan struct{} // local job concurrency limiter
	linuxSem    chan struct{} // Linux dispatch (VM) concurrency limiter
	macSem      chan struct{} // macOS VM concurrency limiter (Vz has a hard cap)
	draining    bool          // true when shutting down, rejects new jobs
	startTime   time.Time

	// retry holds pending re-attempts for jobs whose initial claim
	// failed with a retryable error. Nil when Config.Retry.Enabled=false.
	retry *retryQueue

	// newMacOSVM constructs a per-job macOS VM. Defaults to vm.NewMacOSVM;
	// overridable in tests to inject a fake that exercises the provisioning
	// watchdog without a real Virtualization.framework VM.
	newMacOSVM func(cfg vm.MacOSVMConfig, jobID string) (vm.MacOSVM, error)

	// macUnwindGrace / macStopGrace bound macOS VM teardown on the paths that
	// must not be delayed by it (see slots.go). Zero uses the package
	// defaults; only tests set them, to compress a 30s grace into
	// milliseconds.
	macUnwindGrace time.Duration
	macStopGrace   time.Duration

	// busyProbe overrides the ground-truth "is this runner executing a
	// job" check that vetoes orphan teardown. Nil (the default) uses
	// probeRunnerBusy: local container/VM/process introspection, falling
	// back to the provider's busy flag. Set only by tests, which have no
	// real runtime to introspect.
	busyProbe busyProber
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
	artifactsDir string     // non-empty if OCI artifacts were extracted for this job
	dispatched   string     // non-empty if dispatched to Linux VM worker (stores container name)
	macosVM      vm.MacOSVM // non-nil if running as a macOS VM job
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

	// labelSet is the fungibility class this runner serves (labelSetKey of
	// the dispatching job's labels). An unbound runner is a spare for its
	// WHOLE class, not just for intentKey — the orphan sweep reconciles
	// spares against class demand instead of reaping purely on intentKey.
	labelSet string
}

// trackRunning files a provisioned runner's bookkeeping under its
// dispatch-intent job key and opens a runner-name ledger entry so
// in_progress / completed events can locate the runner by NAME
// regardless of which job the platform actually assigned to it.
//
// labelSet is the dispatching job's fungibility class; it records what the
// runner can still be useful for once its dispatch intent is discharged.
func (s *Scheduler) trackRunning(key jobKey, rj *runningJob, provider providers.Provider, labelSet string) {
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
			labelSet:     labelSet,
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

	s := &Scheduler{
		cfg:        cfg,
		running:    make(map[jobKey]*runningJob),
		seen:       make(map[jobKey]time.Time),
		started:    make(map[jobKey]time.Time),
		pending:    make(map[jobKey]struct{}),
		attempts:   make(map[jobKey]int),
		jobLabels:  make(map[jobKey]string),
		runners:    make(map[string]*runnerBinding),
		sem:        make(chan struct{}, cfg.MaxConcurrent),
		linuxSem:   make(chan struct{}, cfg.MaxConcurrent),
		macSem:     make(chan struct{}, macVMs),
		startTime:  time.Now(),
		newMacOSVM: vm.NewMacOSVM,
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

// bindContexts wires the scheduler's two lifecycle contexts from the run
// context. runCtx is the signal-scoped context: everything that must stop
// promptly on SIGTERM (polling, webhook server, reconcile loop, semaphore
// waits, claim attempts) derives from it. jobsCtx is deliberately DETACHED
// from runCtx's cancellation (values carry over via WithoutCancel): a job
// that is already running keeps its lease across SIGTERM until drain()
// either sees it finish or hits ShutdownTimeout and cancels jobsCtx.
// Without this split, the signal cancels every in-flight job's context the
// instant it arrives, and drain()'s wait loop "succeeds" only because the
// cancellation already killed everything — drain becomes kill.
//
// Split from Run so tests can exercise the same wiring without the full
// serve stack.
func (s *Scheduler) bindContexts(ctx context.Context) {
	jobsCtx, jobsCancel := context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Lock()
	s.runCtx = ctx
	s.jobsCtx = jobsCtx
	s.jobsCancel = jobsCancel
	s.mu.Unlock()
}

// shutdownCh returns a channel closed when the daemon starts going down
// (runCtx cancelled by SIGTERM or by the Windows SCM stop handler). The
// Upgrade RPC hands it to pkg/upgrade so the restart supervisor can tell
// "the restart I asked for is taking effect" from "the restart never
// happened" without racing a slow-but-healthy shutdown. Nil-safe: a
// scheduler that was never Run reports a channel that never closes.
func (s *Scheduler) shutdownCh() <-chan struct{} {
	s.mu.Lock()
	ctx := s.runCtx
	s.mu.Unlock()
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

// jobContext returns the context bounding a single job's runtime, derived
// from jobsCtx (not the signal-scoped run context — see bindContexts) and
// capped at JobTimeout when configured. Every provisioning path uses this
// for the create/wait phase of a runner.
func (s *Scheduler) jobContext() (context.Context, context.CancelFunc) {
	s.mu.Lock()
	base := s.jobsCtx
	s.mu.Unlock()
	if base == nil {
		// Handlers invoked without Run (tests): still never inherit a
		// signal-scoped cancellation.
		base = context.Background()
	}
	if s.cfg.JobTimeout > 0 {
		return context.WithTimeout(base, s.cfg.JobTimeout)
	}
	return context.WithCancel(base)
}

// Cordon marks the scheduler as draining WITHOUT initiating shutdown: new
// queued jobs are rejected while running jobs continue undisturbed. Returns
// the number of jobs still running. Used by the Cordon RPC so
// `ephemerd drain --wait` can stop claims first and only restart the daemon
// once the active job count reaches zero.
//
// The flag is enforced at three points, all of which read it live rather than
// snapshotting it at admission (issue #154):
//
//   - handleQueued — refuses new queued events, whatever their source
//     (webhook, poll, startup catch-up, reconcile sweep, retry-queue fire).
//   - admitDispatch — abandons a dispatch that was accepted before the cordon
//     and then sat blocked on a concurrency semaphore. This is the one that
//     was missing: the node kept provisioning for over a minute after the
//     operator was told it had stopped claiming.
//   - claimJob — the hard backstop. Nothing can register a JIT runner on a
//     cordoned node, even from a dispatch path that skips the gates above.
//
// Cordon means "stop claiming NEW work". Jobs already running are untouched:
// their contexts hang off jobsCtx and are never cancelled here.
func (s *Scheduler) Cordon() int {
	s.mu.Lock()
	s.draining = true
	count := len(s.running)
	s.mu.Unlock()
	metrics.Draining.Set(1)
	return count
}

// Uncordon reverses Cordon: the scheduler resumes claiming queued jobs.
// Returns the number of jobs currently running. Jobs whose queued events
// were rejected while cordoned are picked up again by the next poll or
// reconcile sweep once their seen entry expires (seenTTL).
func (s *Scheduler) Uncordon() int {
	s.mu.Lock()
	s.draining = false
	count := len(s.running)
	s.mu.Unlock()
	metrics.Draining.Set(0)
	return count
}

// ActiveJobs returns the number of jobs currently running. Used by the
// Upgrade RPC (via the upgrade.Drainer interface) to wait for the scheduler
// to drain to idle after a Cordon, the same signal `ephemerd drain --wait`
// polls over the control socket.
func (s *Scheduler) ActiveJobs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running)
}

// Run starts the scheduler. It discovers jobs via polling (default) or
// webhooks (when TLS certs are configured), and manages runner lifecycle.
func (s *Scheduler) Run(ctx context.Context) error {
	// Record the root context for event-driven re-dispatch (reprovisionIfStranded)
	// and derive the detached jobs context. Set once here, before any events are
	// processed or wait-goroutines spawned, so they carry no per-job/retry
	// context values.
	s.bindContexts(ctx)
	defer s.jobsCancel()

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
	// pathOf gives each provider a stable, unique path — normally
	// "/webhook/<name>", but when several providers share a name (one GitHub
	// provider per owner) it disambiguates by owner so they never collide.
	pathOf := webhookPathOf(s.cfg.Providers)
	var whProviders []providers.Webhook
	if useWebhook {
		for _, p := range s.cfg.Providers {
			whp, ok := p.(providers.Webhook)
			if !ok {
				continue
			}
			whProviders = append(whProviders, whp)
			path := pathOf(p)
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
			webhookURL := s.cfg.Tunnel.PublicURL() + pathOf(whp.(providers.Provider))
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

	// External-tunnel webhook auto-registration.
	//
	// With a managed tunnel (Tunnel != nil) the block above already registered
	// hooks against the tunnel's own PublicURL(). The "external" mode has no
	// managed tunnel — ingress is fronted by something else (e.g. a Cloudflare
	// tunnel on another host) — so ephemerd historically only served the
	// receiver and left the operator to hand-add a hook per repo. When
	// ExternalURL is configured we register each webhook-capable provider's
	// hook to <ExternalURL>/webhook/<provider> here instead, mirroring the
	// managed-tunnel block but using the configured URL.
	//
	// Two deliberate differences from the managed path:
	//   - Registration failure does NOT abort startup. The receiver is already
	//     serving; the operator can still add hooks by hand, so we log a WARN
	//     and continue rather than returning an error.
	//   - We do NOT deregister on shutdown. External hooks are operator-owned
	//     and must persist across restarts (unlike the managed tunnel's
	//     ephemeral hooks, which serveTunnelWithReconnect tears down).
	if s.cfg.Tunnel == nil && useWebhook && s.cfg.ExternalURL != "" {
		s.registerExternalWebhooks(ctx, whProviders)
	}

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
		// CatchUpPoll emits into the provider's poll-event channel. In
		// polling mode the Start() forwarder above already drains it, but
		// in webhook mode Start() is never called and NOTHING reads that
		// channel — the caught-up events would sit in the channel buffer
		// forever and the startup recovery would silently do nothing
		// (queued jobs stay queued until a human re-delivers webhooks).
		// Wire the drain before firing the poll.
		if useWebhook {
			if ep, ok := p.(interface {
				Events() <-chan providers.JobEvent
			}); ok {
				go func(ch <-chan providers.JobEvent) {
					for ev := range ch {
						events <- ev
					}
				}(ep.Events())
			} else {
				s.cfg.Log.Warn(
					"provider supports CatchUpPoll but not Events(); startup recovery may be a no-op in webhook mode",
					"provider", p.Name(),
				)
			}
		}
		name := p.Name()
		s.cfg.Log.Info("startup poll: checking for queued jobs", "provider", name)
		go func() {
			if err := catcher.CatchUpPoll(ctx); err != nil {
				s.cfg.Log.Warn("startup poll failed", "provider", name, "error", err)
			}
		}()
	}

	// Webhook mode has no continuous poll. The common stranding case — a
	// fungibly-reassigned runner exiting having run a sibling's job and leaving
	// its own dispatched job queued — is healed instantly and event-drivenly by
	// reprovisionIfStranded on runner exit (see the wait-goroutines). This
	// periodic catch-up poll is only a LAST-RESORT backstop for genuinely
	// DROPPED webhook deliveries, so it runs at a low frequency (default 30m).
	// Swept jobs still flow through handleQueued's seen/running/zombie/canHandle
	// checks, so anything already running or recently handled is skipped (no
	// double-provision), and a genuinely undispatchable job still hits the
	// zombie cap and stops.
	if useWebhook && s.cfg.ReconcileInterval > 0 {
		go s.runReconcileLoop(ctx)
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
			// or inside the embedded Linux VM on macOS — unless the operator
			// turned Linux serving off ([vm.linux] enabled = false).
			osOK = (goruntime.GOOS == "linux" || goruntime.GOOS == "darwin" || s.cfg.LinuxDispatcher != nil) &&
				!s.cfg.LinuxJobsDisabled
		case "windows":
			osOK = goruntime.GOOS == "windows"
		case "macos", "macosx":
			// macOS jobs run in a per-job VM. Accept only when VM config
			// is available on a darwin host.
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
	// NOTE: deliberately NO "already observed running" early-out here. The
	// fungibility check lives in admitDispatch, at the point of claiming, so
	// that a fresh queued event is always allowed to re-enter the pipeline —
	// if the platform says a job is queued, it may genuinely need a runner
	// again. Rejecting here would suppress it for the whole started TTL.
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
	// Record the job's fungibility class so the orphan sweep can tell whether
	// a spare runner still has same-label work it could pick up.
	s.jobLabels[key] = labelSetKey(event.Labels)

	// Admission gate #1: the cordon. Checked BEFORE the zombie counter is
	// touched — a cordon rejection is a refusal to take the job, not a failed
	// provisioning attempt, so it must not burn the job's zombie budget. (It
	// used to sit after the increment: a cordon held for maxProvisionAttempts *
	// seenTTL (~50 min) would permanently mark every rejected job undispatchable
	// even after Uncordon.)
	if s.draining {
		// Drop the pending stamp: it has no TTL, so leaving it would
		// permanently block this job from being handled after Uncordon.
		// The seen entry stays (expires after seenTTL), so an uncordoned
		// scheduler picks the job up again on a later poll/reconcile pass.
		// Keeping it is deliberate: clearing it would let a continuous poll
		// re-enter this path every interval instead of once per seenTTL.
		delete(s.pending, key)
		s.mu.Unlock()
		log.Info("rejecting job, scheduler is draining")
		return
	}

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
	s.mu.Unlock()

	// Dispatch Linux jobs to the Linux VM worker if available
	if s.cfg.LinuxDispatcher != nil && isLinuxJob(event.Labels) {
		s.handleLinuxJob(ctx, event)
		return
	}

	// Route macOS jobs to a per-job VM (the only macOS path).
	if isMacOSJob(event.Labels) {
		s.mu.Lock()
		macCfg := s.cfg.MacOSVMConfig
		s.mu.Unlock()
		if macCfg != nil {
			s.handleMacOSJob(ctx, event)
			return
		}
		// VM not available yet — remove from seen/pending so the next
		// poll retries this job once the install finishes.
		s.mu.Lock()
		delete(s.seen, key)
		delete(s.pending, key)
		s.mu.Unlock()
		log.Info("macOS runner not ready, deferring job")
		return
	}

	s.handleLocalJob(ctx, event)
}

// dispatchVerdict is the outcome of the post-slot admission gate. Anything
// other than dispatchAdmit means the caller must release its concurrency slot
// and return WITHOUT provisioning.
type dispatchVerdict int

const (
	// dispatchAdmit: go ahead and provision.
	dispatchAdmit dispatchVerdict = iota
	// dispatchAbandonSatisfied: a fungible sibling runner already ran this job.
	dispatchAbandonSatisfied
	// dispatchAbandonCordoned: the node was cordoned while this dispatch
	// waited for a concurrency slot.
	dispatchAbandonCordoned
)

// log emits the verdict's explanation on the caller's job-scoped logger, so
// every provisioning path reports an abandonment identically.
func (v dispatchVerdict) log(log *slog.Logger) {
	switch v {
	case dispatchAbandonSatisfied:
		log.Info("abandoning dispatch: job was observed running while this dispatch waited for a concurrency slot",
			"detail", "same-label JIT runners are fungible; a sibling runner took this job, so provisioning now would create an orphan")
	case dispatchAbandonCordoned:
		log.Info("abandoning dispatch: scheduler was cordoned while this dispatch waited for a concurrency slot",
			"detail", "cordon means stop claiming NEW work; the job stays queued for another node or for this node after uncordon")
	}
}

// admitDispatch is the last gate before a provisioning path claims a runner.
// Every path calls it immediately after acquiring its concurrency slot, in
// place of the bare pending-map cleanup it replaces. It always clears
// pending[key]; it returns a non-admit verdict when this dispatch must be
// ABANDONED. The caller must release its concurrency slot on abandon.
//
// TWO windows close here, both of the same shape: the decision to provision is
// taken in handleQueued, but provisioning happens later — after the path has
// blocked on the concurrency semaphore, which on a max_concurrent = 1 node is
// the entire duration of whatever is already running. State can change
// underneath a blocked dispatch, so it is re-checked here rather than trusted
// from admission time.
//
//  1. Fungibility (dispatchAbandonSatisfied). GitHub treats same-label JIT
//     runners as interchangeable and hands an ALREADY-DISPATCHED runner this
//     very job; it runs and completes. Without this check the path would go on
//     to register a fresh JIT runner for a job that finished minutes ago. That
//     runner never binds, so no completed event tears it down, and it holds the
//     only concurrency slot until the orphan sweep's grace window expires.
//
//  2. Cordon (dispatchAbandonCordoned). This is issue #154: `drain`/`Cordon`
//     set draining=true and the operator was told the node had stopped claiming,
//     but a job admitted seconds earlier was still parked on the semaphore. When
//     the slot freed it sailed past the (already-passed) handleQueued check and
//     registered a JIT runner 82 seconds after the cordon was acknowledged, so
//     the node never quiesced. Re-reading the flag here is what makes the cordon
//     an actual gate rather than an admission-time snapshot.
//
// Intended behaviour for a job accepted before the cordon but not yet
// provisioned: it is ABANDONED, not provisioned and not queued for later. The
// pending stamp is cleared (it has no TTL and would otherwise block the job
// forever) and the seen stamp is deliberately left in place to age out via
// seenTTL — identical to the handleQueued rejection, so a job rejected at
// either gate behaves the same. The job itself is untouched on the platform: it
// stays queued, so another node can take it immediately, or this node picks it
// up after Uncordon on the next poll/reconcile/catch-up pass.
//
// Jobs ALREADY RUNNING are unaffected — this gate is only ever consulted on the
// path to a new claim, never on a running job's lifecycle.
func (s *Scheduler) admitDispatch(key jobKey) dispatchVerdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, key)
	if s.draining {
		return dispatchAbandonCordoned
	}
	if !s.webhookMode {
		// started is only populated from in_progress/completed webhooks.
		return dispatchAdmit
	}
	if _, done := s.started[key]; done {
		return dispatchAbandonSatisfied
	}
	return dispatchAdmit
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
	slot := s.acquireSlot(ctx, s.linuxSem, "linux", log)
	if slot == nil {
		unsee()
		return
	}
	// Backstop until the wait-goroutine below takes ownership: every exit
	// from this function returns the slot, including one a future edit
	// forgets to release explicitly. See slots.go — a single missed release
	// on the macOS path cost 28 hours of CI (#196).
	handedOff := false
	defer func() {
		if !handedOff {
			slot.release()
		}
	}()

	if v := s.admitDispatch(key); v != dispatchAdmit {
		v.log(log)
		slot.release()
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
		log.Error("failed to claim job", "error", err, "error_class", classifyErr(err))
		unsee()
		slot.release()
		// Replaces the old blind time.Sleep(backoffDuration): the
		// sem is released FIRST so we do not hold a slot idle across
		// the wait, then a rate-aware jittered retry is enqueued
		// (no-op when Retry.Enabled is false or the error is not
		// retryable).
		s.enqueueRetryIfEligible(ctx, event, err)
		return
	}

	// Derived from jobsCtx, not ctx: the job keeps running across SIGTERM
	// until it finishes or drain() gives up (see bindContexts).
	jobCtx, cancel := s.jobContext()

	if err := s.cfg.LinuxDispatcher.Create(jobCtx, claim.RunnerName, image, claim.RunnerConfig, event.Provider.Name(), event.Repo); err != nil {
		log.Error("dispatch create failed", "error", err, "error_class", classifyErr(err))
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		unsee()
		cancel()
		slot.release()
		// Mirror the claim-failure branch above (and the local create
		// path): a failed dispatch create used to drop the job forever —
		// webhooks are never re-delivered. Slot released first; no-op for
		// non-retryable errors or while draining.
		s.enqueueRetryIfEligible(ctx, event, err)
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
	}, event.Provider, labelSetKey(event.Labels))

	log.Info("Linux runner dispatched", "name", claim.RunnerName)

	// Wait for the job to finish in the background
	handedOff = true
	go func() {
		// Same slot-release ordering as the local wait-goroutine: release
		// once the job is untracked, never behind the destroy. The
		// deferred call is only the panic/early-return backstop.
		defer slot.release()

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

		// Release BEFORE the destroy RPC, mirroring the local path: the
		// dispatched env is keyed by its unique claim.RunnerName, so a
		// newly admitted job cannot collide with this teardown.
		slot.release()

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

		// Self-heal: if this runner exited without its dispatched job ever
		// being observed to run (fungibly reassigned to a sibling), re-provision.
		s.reprovisionIfStranded(ctx, event)
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
	slot := s.acquireSlot(ctx, s.macSem, "macos", log)
	if slot == nil {
		unsee()
		return
	}
	// Backstop until the wait-goroutine below takes ownership. This function
	// is issue #196's crime scene: it had six hand-written `<-s.macSem`
	// returns and one of them was unreachable, because the provisioning
	// watchdog it depended on could block forever before returning. The
	// deferred release makes "did every path give the slot back?" a property
	// of the function rather than of the reader's attention span.
	handedOff := false
	defer func() {
		if !handedOff {
			slot.release()
		}
	}()

	if v := s.admitDispatch(key); v != dispatchAdmit {
		v.log(log)
		slot.release()
		return
	}

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
		slot.release()
		s.enqueueRetryIfEligible(ctx, event, err)
		return
	}

	// Create the macOS VM
	macVM, err := s.newMacOSVM(*s.cfg.MacOSVMConfig, fmt.Sprintf("%d", jobID))
	if err != nil {
		log.Error("failed to create macOS VM", "error", err)
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		unsee()
		slot.release()
		return
	}

	// Write JIT config to the shared directory before booting
	if err := macVM.WriteJITConfig(claim.RunnerConfig); err != nil {
		log.Error("failed to write JIT config", "error", err)
		// Slot first, teardown second — for the rest of this function every
		// failure path releases before it touches the VM, because Stop() is
		// the call that hangs (#196).
		unsee()
		slot.release()
		stopVMBounded(macVM, s.teardownGrace(), log)
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		return
	}

	// Derived from jobsCtx, not ctx: the job keeps running across SIGTERM
	// until it finishes or drain() gives up (see bindContexts).
	jobCtx, cancel := s.jobContext()

	// Boot the VM
	if err := macVM.Start(jobCtx); err != nil {
		log.Error("failed to start macOS VM", "error", err)
		unsee()
		cancel()
		slot.release()
		stopVMBounded(macVM, s.teardownGrace(), log)
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		return
	}

	// Wait for the runner inside the VM to become reachable, bounded by a
	// hard provisioning deadline. Without this bound a hung guest SSH command
	// can wedge WaitForRunner indefinitely: the job never gets tracked (so it
	// is invisible to `ephemerd jobs` and the orphan sweep) yet still holds
	// the sole macOS slot, stalling all macOS CI until a human intervenes.
	ip, err := s.waitForMacRunnerBounded(jobCtx, macVM, log)
	if err != nil {
		log.Error("macOS VM runner not reachable", "error", err)
		// THE 28-HOUR LINE (#196). The slot goes back here, first, before
		// anything that talks to the VM or to GitHub. It used to go back
		// last, after macVM.Stop() — and waitForMacRunnerBounded had already
		// force-stopped this VM, so this Stop() re-entered a sync.Once whose
		// first call was still wedged inside Virtualization.framework and
		// blocked forever. The node logged "force-stopping VM to reclaim the
		// slot", never reclaimed it, and starved every subsequent macOS job
		// while reporting status: ok / active_jobs: 0.
		unsee()
		cancel()
		slot.release()
		stopVMBounded(macVM, s.teardownGrace(), log)
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
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
	}, event.Provider, labelSetKey(event.Labels))

	log.Info("macOS VM runner ready", "name", claim.RunnerName, "ip", ip)

	// Wait for the job to finish in the background
	handedOff = true
	go func() {
		// Backstop only. The release that matters happens the moment the job
		// is untracked, below.
		defer slot.release()

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

		// Clean up. The entry may already be gone if a completed event tore
		// this VM down by name (see handleCompleted).
		s.mu.Lock()
		rj, exists := s.running[key]
		if exists {
			s.untrackRunningLocked(key, rj)
		}
		s.mu.Unlock()

		// Release BEFORE teardown, exactly as the local and Linux
		// wait-goroutines do (PR #190). The job is untracked, so its demand
		// is gone; everything below can block for a long time and none of it
		// may delay the next macOS job. macVM.Stop() waits up to 15s for a
		// graceful guest shutdown before forcing (and on the #196 node the
		// force did not return either), and ReleaseJob is a GitHub API call
		// on context.Background() with no deadline at all. This is safe
		// against a newly admitted job: every VM, its clone, and its shared
		// directory are keyed by job ID, so a fresh dispatch cannot touch
		// anything this teardown still owns.
		slot.release()

		if exists {
			metrics.JobsActive.Dec()
			if rj.macosVM != nil {
				stopVMBounded(rj.macosVM, s.teardownGrace(), log)
			}
			if rj.artifactsDir != "" {
				artifacts.Cleanup(rj.artifactsDir, s.cfg.Log)
			}
			if rj.provider != nil && rj.claim != nil {
				if err := rj.provider.ReleaseJob(context.Background(), rj.claim); err != nil {
					log.Debug("deregister runner after macOS VM cleanup", "error", err)
				}
			}
		}

		// Self-heal: re-provision if this VM's dispatched job never ran.
		s.reprovisionIfStranded(ctx, event)
	}()
}

// waitForMacRunnerBounded runs macVM.WaitForRunner under a hard provisioning
// deadline and guarantees it returns.
//
// The VM-internal wait already caps its own polling loop at ~2 minutes, but
// once the guest's SSH port opens it shells in to start the runner, and those
// golang.org/x/crypto/ssh session calls carry no deadline and do not observe
// ctx — a guest command that never returns wedges the wait forever. When that
// happens the job is stuck BEFORE trackRunning: it is absent from `s.running`
// (invisible to `ephemerd jobs`) and from `s.runners` (invisible to the orphan
// sweep), yet still holds the single macOS concurrency slot, so every later
// macOS job silently starves.
//
// The only reliable way to interrupt a blocked SSH session call is to drop the
// connection: force-stopping the VM tears down the guest, the SSH read errors
// out, and WaitForRunner unwinds. On timeout we do exactly that and report a
// failure; the caller's error path releases the slot and deregisters the
// runner, so cleanup runs even though the wait had been stuck.
//
// ISSUE #196 — why the teardown is now BOUNDED. The first version of this
// function force-stopped the VM and then did a bare `<-resCh`, waiting for the
// wait goroutine to unwind "so there is no leak". That assumed killing the VM
// always unblocks the guest wait. On the fleet's mac it did not: the VM's own
// stop path timed out ("macOS VM did not stop gracefully, forcing") and the
// wait never returned, so this function — which had just logged that it was
// force-stopping the VM TO RECLAIM THE SLOT — parked on that receive for 28
// hours, still holding the slot. The job had never been tracked, so
// active_jobs stayed 0, /healthz stayed 200, and five macOS jobs queued behind
// it aged out at GitHub's 24-hour limit without ever running.
//
// A wedged guest wait is already a leaked goroutine that nothing in this
// process can kill. resCh is buffered so that goroutine can always complete
// its send and exit if it ever comes back. What must NOT happen is turning
// that leaked goroutine into a leaked concurrency slot: teardown gets a
// bounded grace period to be tidy, and then we return regardless.
func (s *Scheduler) waitForMacRunnerBounded(ctx context.Context, macVM vm.MacOSVM, log *slog.Logger) (string, error) {
	timeout := s.cfg.MacOSProvisionTimeout
	if timeout <= 0 {
		timeout = defaultMacOSProvisionTimeout
	}

	type result struct {
		ip  string
		err error
	}
	// Buffered: the wait goroutine must always be able to finish its send and
	// exit, even long after we have stopped listening. Nothing below may
	// depend on it ever getting that far.
	resCh := make(chan result, 1)
	unwound := make(chan struct{})
	go func() {
		defer close(unwound)
		ip, err := macVM.WaitForRunner(ctx)
		resCh <- result{ip: ip, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case r := <-resCh:
		return r.ip, r.err
	case <-timer.C:
		log.Error("macOS VM stuck in provisioning past deadline; force-stopping VM to reclaim the slot", "timeout", timeout)
		s.abandonMacProvision(macVM, unwound, log)
		return "", fmt.Errorf("timed out after %s waiting for macOS VM runner to become reachable", timeout)
	case <-ctx.Done():
		s.abandonMacProvision(macVM, unwound, log)
		return "", ctx.Err()
	}
}

// abandonMacProvision force-stops a macOS VM whose provisioning was given up
// on and gives the teardown a bounded chance to finish before returning.
//
// Stop() runs on its own goroutine for two reasons. The obvious one is that
// Virtualization.framework's stop can itself hang. The subtle one is
// sync.Once: darwinMacOSVM.Stop is `stopOnce.Do(m.stop)`, so a SECOND caller
// does not return early — it blocks until the first invocation returns. The
// caller's error path stops the VM again as part of its normal cleanup, and if
// this teardown were still wedged in the Once, that "cleanup" call would hang
// on a path that is holding a concurrency slot. Off-thread here plus
// stopVMBounded there means neither can pin the slot.
func (s *Scheduler) abandonMacProvision(macVM vm.MacOSVM, unwound <-chan struct{}, log *slog.Logger) {
	// Dropping the guest is what unblocks a wedged SSH session call.
	stopped := stopVMAsync(macVM)
	if !awaitUnwind(stopped, unwound, s.provisionUnwindGrace()) {
		log.Error("macOS VM teardown did not finish after the force-stop; abandoning it and reclaiming the concurrency slot anyway",
			"grace", s.provisionUnwindGrace(),
			"detail", "a wedged Vz stop or a wedged guest wait must never pin the macOS slot — see issue #196")
	}
}

// stopVMAsync stops macVM on its own goroutine and returns a channel closed
// when Stop returns. If Stop never returns, that goroutine is leaked — which
// is the correct trade: the alternative is leaking the concurrency slot, and
// a leaked slot takes the node's whole macOS capacity with it.
func stopVMAsync(macVM vm.MacOSVM) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		macVM.Stop()
	}()
	return done
}

// stopVMBounded force-stops a VM without letting the teardown pin the caller.
// Used by every macOS cleanup path that runs while a slot could still be held
// or a job could still be waiting on this goroutine.
func stopVMBounded(macVM vm.MacOSVM, grace time.Duration, log *slog.Logger) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-stopVMAsync(macVM):
	case <-timer.C:
		log.Warn("macOS VM stop did not return within the grace period; continuing without it",
			"grace", grace)
	}
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
	slot := s.acquireSlot(ctx, s.sem, "local", log)
	if slot == nil {
		unsee()
		return
	}
	// Backstop until the wait-goroutine below takes ownership; see the
	// matching comment in handleLinuxJob and slots.go.
	handedOff := false
	defer func() {
		if !handedOff {
			slot.release()
		}
	}()

	if v := s.admitDispatch(key); v != dispatchAdmit {
		v.log(log)
		slot.release()
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
		log.Error("failed to claim job", "error", err, "error_class", classifyErr(err))
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		unsee()
		slot.release()
		// Replaces the old blind 5s sleep with a rate-aware jittered
		// retry. No-op when Retry is disabled or the error is not
		// retryable.
		s.enqueueRetryIfEligible(ctx, event, err)
		return
	}

	// Create the runner environment with job timeout.
	// Derived from jobsCtx, not ctx: the job keeps running across SIGTERM
	// until it finishes or drain() gives up (see bindContexts).
	//
	// Create is retried IN PLACE for transient failures (a dead shim's
	// "ttrpc: closed", a containerd restart, ...): the slot is held and
	// the JIT claim already minted, so a couple of short-backoff attempts
	// here are far cheaper than release-and-requeue. The retry queue
	// below is the real safety net when these run out.
	jobCtx, cancel := s.jobContext()
	var env *runtime.RunnerEnv
	err = retryEnvCreate(jobCtx, log, createEnvBackoffs,
		func(err error) bool { return classifyErr(err) != errNonRetryable },
		s.isDraining,
		func() error {
			var cerr error
			env, cerr = s.cfg.Runtime.Create(jobCtx, runtime.CreateConfig{
				ID:         claim.RunnerName,
				Image:      image,
				JITConfig:  claim.RunnerConfig,
				Env:        claim.Env,
				Entrypoint: claim.Entrypoint,
			})
			return cerr
		})
	if err != nil {
		log.Error("failed to create runner environment", "error", err, "error_class", classifyErr(err))
		// Remove the ghost runner since the container won't start
		if rmErr := event.Provider.ReleaseJob(ctx, claim); rmErr != nil {
			log.Warn("failed to remove ghost runner", "runner_id", claim.RunnerID, "error", rmErr)
		}
		if artifactsDir != "" {
			artifacts.Cleanup(artifactsDir, s.cfg.Log)
		}
		unsee()
		cancel()
		slot.release()
		// Mirror the claim-failure branch above: without this a job whose
		// env create failed was dropped forever (GitHub never re-delivers
		// the webhook) — on metal that was 1h43m of a job sitting queued
		// after a dead shim failed a single create. Slot released FIRST so
		// the retry never fires while we still hold it; no-op when the
		// error is non-retryable or the node is draining.
		s.enqueueRetryIfEligible(ctx, event, err)
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
	}, event.Provider, labelSetKey(event.Labels))

	log.Info("runner environment ready", "name", claim.RunnerName)

	// Wait for the job to finish in the background
	handedOff = true
	go func() {
		// Slot release is decoupled from teardown. It used to hang off a
		// bare defer, which meant the slot came back only after Destroy
		// returned — so a wedged teardown (dead shim) pinned a
		// MaxConcurrent slot for its whole hang. The slot is released as
		// soon as the job is untracked below; the deferred call is only
		// the panic/early-return backstop.
		defer slot.release()

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
		}
		s.mu.Unlock()

		// Release the concurrency slot BEFORE destroying: the job is
		// untracked, so its demand is gone, and the teardown must not be
		// able to delay the next job's admission (Destroy is bounded, but
		// even a bounded 5m hang is 5m of a dead slot). This is safe
		// against a new job racing this env's teardown: every env is
		// keyed by its unique claim.RunnerName — container ID, per-job
		// runner dir, netns/HCN endpoint, dind socket all derive from it —
		// so a freshly admitted job cannot touch anything this teardown
		// still owns.
		slot.release()

		if exists {
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
		}

		// Self-heal: re-provision if this runner's dispatched job never ran.
		s.reprovisionIfStranded(ctx, event)
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

	// The job actually started running somewhere (on our runner, a peer
	// daemon's, or a fungibly-reassigned sibling runner). Record it as
	// satisfied so reprovisionIfStranded won't re-provision it when the
	// runner we originally dispatched for it exits. This is the core of the
	// event-driven self-heal: a job is "handled" when we OBSERVE it start,
	// not when we optimistically dispatch a runner for it.
	s.mu.Lock()
	s.started[key] = time.Now()
	s.mu.Unlock()

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
	// The job reached a terminal state — definitely don't re-provision it,
	// even if its dispatch-intent runner exits afterwards. Records the same
	// satisfied signal as in_progress in case the in_progress delivery was
	// missed (a cancelled-before-start job goes queued -> completed with no
	// in_progress at all).
	s.started[key] = time.Now()
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

	// A job reaching a terminal state removes demand from its fungibility
	// class, which can leave a same-label spare runner with nothing left to
	// do. Reconcile now rather than waiting for the 5-minute sweep tick (and
	// certainly rather than the orphan grace window). Deferred so it observes
	// the teardown below; a no-op when the sweep is disabled or in poll mode.
	defer s.sweepOrphanRunners()

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
//
// The wait is real: job contexts hang off jobsCtx, which the signal that
// triggered this drain did NOT cancel (see bindContexts), so s.running
// empties because jobs complete — their wait-goroutines see the runner
// exit and untrack them — not because cancellation killed them. jobsCtx
// is only canceled on the way out, after the wait or the force-kill.
func (s *Scheduler) drain() {
	s.mu.Lock()
	s.draining = true
	count := len(s.running)
	jobsCancel := s.jobsCancel
	s.mu.Unlock()
	metrics.Draining.Set(1)

	// Whichever way drain exits, release the job lease so nothing derived
	// from jobsCtx outlives the daemon. Nil when Run was never started.
	if jobsCancel != nil {
		defer jobsCancel()
	}

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

	// held_slots/slot_capacity/slots are ADDITIVE — active_jobs and
	// max_concurrent keep their exact meaning for anything already scraping
	// this endpoint.
	//
	// active_jobs counts TRACKED jobs (len(s.running)). A job that is
	// provisioning, or one whose slot leaked, is not tracked, so a node with
	// every slot held reported active_jobs: 0 and looked idle — that is how
	// the #196 macOS outage stayed invisible for 28 hours while /healthz
	// returned 200. held_slots is read straight off the semaphores and so
	// counts the capacity that is actually spoken for. held == capacity with
	// active_jobs 0, sustained, means a stuck provision or a leak.
	status := map[string]any{
		"status":         "ok",
		"active_jobs":    activeJobs,
		"max_concurrent": s.cfg.MaxConcurrent,
		"held_slots":     s.HeldSlots(),
		"slot_capacity":  s.SlotCapacity(),
		"slots":          s.SlotUsage(),
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
			// The job is no longer demand for its fungibility class.
			delete(s.jobLabels, id)
		}
	}
	// Prune the started (satisfied) set. It must outlive the LONGEST runner in
	// a cohort: a sibling runner dispatched for job X that instead ran a long
	// job doesn't exit — and thus can't falsely re-provision the already-run
	// X — until its own job finishes, up to JobTimeout after dispatch. Keep
	// started entries at least that long (plus a margin) so the satisfied
	// signal is still present when that late exit checks it.
	startedTTL := s.cfg.JobTimeout + seenTTL
	if s.cfg.JobTimeout <= 0 {
		// No job timeout configured: fall back to GitHub's own max job runtime.
		startedTTL = 6 * time.Hour
	}
	for id, t := range s.started {
		if time.Since(t) > startedTTL {
			delete(s.started, id)
		}
	}
}

// orphanVictim is a runner the sweep has decided to tear down, already
// unhooked from the bookkeeping maps. discharged distinguishes the two
// reasons: false = never assigned anything within the grace window (the
// original intent-keyed rule); true = its job ran on a same-label sibling
// and no queued job in that label set needs it (the label-set rule).
type orphanVictim struct {
	name       string
	key        jobKey
	rj         *runningJob
	discharged bool

	// escaped records that teardown proceeded over a busy veto that never
	// cleared. Logged loudly — this is the only remaining path by which
	// the sweep can kill a live job.
	escaped bool
	// verdict / reason carry the busy check's answer into the log line.
	verdict string
	reason  string
}

// orphanNomination is a runner the sweep's rules have PROPOSED for
// teardown. It is not yet a victim: the bookkeeping maps still hold it,
// and the busy check gets a veto before anything is unhooked.
//
// Nominating and reaping are deliberately separate phases because the
// busy check does I/O (a containerd query, an SSH round trip, a GitHub
// GET) and must not run under s.mu — and because in the window while it
// runs, an in_progress webhook may arrive and bind the runner, which the
// reap phase re-checks.
type orphanNomination struct {
	name       string
	rb         *runnerBinding
	rj         *runningJob
	discharged bool
}

// nominateRunnerLocked resolves a ledger entry to the job it belongs to
// without unhooking anything. Caller holds s.mu. Returns ok=false when the
// entry is stale or no longer names this runner.
func (s *Scheduler) nominateRunnerLocked(name string, rb *runnerBinding) (orphanNomination, bool) {
	rj, ok := s.running[rb.intentKey]
	if !ok {
		// Stale ledger entry — the wait-goroutine already cleaned up.
		delete(s.runners, name)
		return orphanNomination{}, false
	}
	if rj.runnerName() != name {
		return orphanNomination{}, false
	}
	return orphanNomination{name: name, rb: rb, rj: rj}, true
}

// reapRunnerLocked unhooks a runner from the bookkeeping maps and returns it
// for teardown. Caller holds s.mu. Returns ok=false when the ledger entry is
// stale, no longer names this runner, or has been bound to a job since it was
// nominated — the last case is the race the nominate/veto split exists to
// catch: an in_progress webhook landing while the busy check was in flight.
func (s *Scheduler) reapRunnerLocked(name string, rb *runnerBinding) (orphanVictim, bool) {
	if cur, ok := s.runners[name]; !ok || cur != rb {
		// The ledger entry was replaced or removed while the busy check
		// ran; whatever is there now was not what we nominated.
		return orphanVictim{}, false
	}
	if rb.bound {
		return orphanVictim{}, false
	}
	rj, ok := s.running[rb.intentKey]
	if !ok {
		// Stale ledger entry — the wait-goroutine already cleaned up.
		delete(s.runners, name)
		return orphanVictim{}, false
	}
	if rj.runnerName() != name {
		return orphanVictim{}, false
	}
	s.untrackRunningLocked(rb.intentKey, rj)
	return orphanVictim{name: name, key: rb.intentKey, rj: rj}, true
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
//
// Two rules retire a runner:
//
//  1. Intent-keyed (original): unbound for longer than Grace. This is the
//     backstop for a runner nothing ever wanted — it must stay, or a genuinely
//     unassigned runner would leak forever.
//
//  2. Label-set (fungibility reconciliation): unbound, and the job it was
//     dispatched for has since been OBSERVED running somewhere else, and its
//     fungibility class has no queued job left that could still be handed to
//     it. Such a runner is discharged — waiting out the grace window for it
//     just burns a concurrency slot (90 minutes of one, on the max_concurrent
//     = 1 macOS node where this was diagnosed).
//
// Rule 2 counts rather than guesses: spares in a class are allocated against
// that class's uncovered queued jobs first, so a spare that GitHub can still
// legitimately hand a sibling job is kept (and no second runner is dispatched
// for that job), while genuine surplus is retired immediately.
//
// # Both rules only NOMINATE
//
// Both rules decide from the same evidence — the scheduler's belief about
// which runner is bound, assembled from in_progress webhooks — and that
// belief is inference, not observation. A same-label burst permutes three
// JIT runners across three jobs, and between the first in_progress
// delivery and the last there is a window in which a runner that is
// already executing a build still looks unbound; every completed event
// triggers a sweep, so landing in that window is routine. Reaping on that
// belief killed live builds.
//
// So the rules produce NOMINATIONS, and a busy check taken at the moment
// of teardown — not derived from event history — either confirms or
// vetoes each one. See busy.go for the check and its (time-bounded)
// escape hatches.
func (s *Scheduler) sweepOrphanRunners() {
	if !s.cfg.OrphanSweep.Enabled {
		return
	}
	policy := s.reapPolicy()
	grace := policy.Grace

	var nominations []orphanNomination

	s.mu.Lock()
	if !s.webhookMode {
		s.mu.Unlock()
		return
	}

	// Demand per fungibility class: jobs we accepted, have NOT observed
	// running, and for which no live runner has been dispatched. Those are
	// the jobs a spare same-label runner could still legitimately be handed
	// by GitHub, so a spare covering one of them has not lost its purpose.
	served := make(map[jobKey]struct{}, len(s.runners))
	for _, rb := range s.runners {
		served[rb.intentKey] = struct{}{}
	}
	demand := make(map[string]int)
	for key := range s.seen {
		if _, done := s.started[key]; done {
			continue
		}
		if _, covered := served[key]; covered {
			continue
		}
		if s.attempts[key] > maxProvisionAttempts {
			// Undispatchable zombie: we have given up on it, so it is not
			// demand and must not keep a spare runner alive.
			continue
		}
		demand[s.jobLabels[key]]++
	}

	// spares are unbound runners whose dispatch intent has been DISCHARGED —
	// the job they were brought up for was observed running elsewhere (a
	// sibling runner or a peer daemon took it). They are still inside the
	// grace window, so the intent-keyed rule alone would leave them idle for
	// the whole window; the label-set rule retires the ones no queued job in
	// their class can use.
	type spare struct {
		name string
		rb   *runnerBinding
	}
	var spares []spare

	for name, rb := range s.runners {
		if rb.bound || !rb.observable {
			continue
		}
		if time.Since(rb.dispatchedAt) >= grace {
			if n, ok := s.nominateRunnerLocked(name, rb); ok {
				nominations = append(nominations, n)
			}
			continue
		}
		if _, discharged := s.started[rb.intentKey]; discharged {
			spares = append(spares, spare{name: name, rb: rb})
		}
	}

	// Allocate the class's remaining demand to the NEWEST spares (they have
	// the most grace left to be picked up) and retire whatever is left over.
	sort.Slice(spares, func(i, j int) bool {
		return spares[i].rb.dispatchedAt.After(spares[j].rb.dispatchedAt)
	})
	for _, sp := range spares {
		name, rb := sp.name, sp.rb
		if demand[rb.labelSet] > 0 {
			// Still the cheapest way to serve a queued same-label job:
			// GitHub will hand this runner that job without us dispatching
			// another one. Keep it and count it against the class.
			demand[rb.labelSet]--
			continue
		}
		if n, ok := s.nominateRunnerLocked(name, rb); ok {
			n.discharged = true
			nominations = append(nominations, n)
		}
	}
	s.mu.Unlock()

	victims := s.vetoBusyNominations(nominations, policy)

	for _, v := range victims {
		switch {
		case v.escaped:
			s.cfg.Log.Warn("ESCAPE: destroying a runner the busy check never cleared — it is wedged, or the busy check is broken",
				"runner", v.name,
				"dispatched_for_job", v.key.JobID,
				"busy_verdict", v.verdict,
				"reason", v.reason,
				"grace", grace,
				"hard_bound", policy.HardBound)
		case v.discharged:
			s.cfg.Log.Info("retiring discharged runner: its job ran on a same-label sibling and no queued job in that label set needs it",
				"runner", v.name,
				"dispatched_for_job", v.key.JobID,
				"busy_verdict", v.verdict,
				"detail", "same-label JIT runners are fungible; reconciled on the label set instead of waiting out the grace window")
		default:
			s.cfg.Log.Warn("destroying orphaned runner: dispatched but never assigned a job within the grace window",
				"runner", v.name,
				"dispatched_for_job", v.key.JobID,
				"busy_verdict", v.verdict,
				"grace", grace)
		}
		metrics.JobsActive.Dec()
		v.rj.cancel()
		if v.rj.macosVM != nil {
			v.rj.macosVM.Stop()
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
	// Cordoned nodes take on no new retry work. A retry scheduled now would
	// fire minutes later against a node that is draining (or, during an
	// upgrade, already gone), and every fire re-enters handleQueued only to be
	// rejected. Outstanding retries for jobs already in the queue are dropped
	// by classifyErr's errCordoned case as they fail through.
	s.mu.Lock()
	draining := s.draining
	s.mu.Unlock()
	if draining {
		return
	}
	s.retry.Add(event, s.retryHandler, err)
}

// isDraining reports whether the scheduler has stopped taking new work
// (shutdown drain or operator cordon).
func (s *Scheduler) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// createEnvBackoffs is the short in-place backoff ladder for a failed
// runner-environment create. Deliberately tiny: the caller holds a
// concurrency slot and a minted JIT claim for the whole ladder, so this
// only rides out blips (a shim crash mid-create, a containerd restart) —
// anything longer-lived goes through release-and-requeue on the retry
// queue instead.
var createEnvBackoffs = []time.Duration{5 * time.Second, 15 * time.Second}

// retryEnvCreate runs create, retrying in place up to len(backoffs)
// extra times with the given backoff between attempts. It gives up early
// — returning the last error — when:
//   - retryable(err) is false (a permanent error won't heal in 15s), or
//   - drained() is true (a draining node must not sit on a slot waiting
//     to start NEW work; the caller releases and returns, as it always
//     did), or
//   - ctx is cancelled mid-backoff (shutdown, job timeout).
//
// Pure aside from the injected callbacks so the attempt/short-circuit
// behavior is unit-testable without a Scheduler.
func retryEnvCreate(ctx context.Context, log *slog.Logger, backoffs []time.Duration,
	retryable func(error) bool, drained func() bool, create func() error) error {
	err := create()
	for i := 0; err != nil && i < len(backoffs); i++ {
		if !retryable(err) || drained() {
			return err
		}
		log.Warn("failed to create runner environment; retrying in place",
			"error", err, "attempt", i+1, "backoff", backoffs[i])
		timer := time.NewTimer(backoffs[i])
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return err
		}
		// Drain may have started during the backoff; re-check before
		// spending another create against a node that stopped taking work.
		if drained() {
			return err
		}
		err = create()
	}
	return err
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

// reprovisionIfStranded re-provisions a job whose dispatched runner has just
// exited WITHOUT that job ever being observed to start. It is called at the end
// of every wait-goroutine (i.e. on every runner exit).
//
// This is the event-driven fix for the fungible-runner stranding problem.
// GitHub treats same-label JIT runners as interchangeable, so the runner we
// dispatched "for" job A routinely runs a sibling job B instead. When that
// runner exits (having run B), A may still be queued with no runner to run it.
// handleQueued marked A "seen" on the QUEUED event — an optimistic bet that the
// runner we brought up would run A — so nothing else re-provisions it and A sits
// queued until the seen entry ages out (~10m) or the low-frequency reconcile
// poll happens to sweep it. Instead, we react to the event we already have: the
// runner exit. If we never observed A go in_progress or completed (started[A]
// unset), A never actually ran, so we clear its seen dedup and re-dispatch it
// immediately — no polling, nothing lost.
//
// Guards:
//   - Webhook mode only. in_progress/completed events (which set started) are
//     only observable via webhooks; in poll mode "never started" is meaningless
//     and the continuous poll already reconciles stranded jobs.
//   - started[key] set => the job ran (on our runner, a peer's, or a sibling) —
//     satisfied, do nothing.
//   - running/pending[key] set => already being (re-)handled, don't double.
//   - attempts over the zombie cap => undispatchable, stop (a superseded run
//     would otherwise re-provision on every runner exit forever).
//
// Re-dispatch is launched in its own goroutine (mirroring the Run loop's
// `go s.handleQueued`) so it never blocks on the concurrency slot the exiting
// wait-goroutine is about to release.
func (s *Scheduler) reprovisionIfStranded(ctx context.Context, event providers.JobEvent) {
	key := keyFor(event)

	s.mu.Lock()
	if !s.webhookMode || s.draining {
		s.mu.Unlock()
		return
	}
	if _, started := s.started[key]; started {
		s.mu.Unlock()
		return
	}
	if _, ok := s.running[key]; ok {
		s.mu.Unlock()
		return
	}
	if _, ok := s.pending[key]; ok {
		s.mu.Unlock()
		return
	}
	if s.attempts[key] > maxProvisionAttempts {
		s.mu.Unlock()
		return
	}
	// Clear the seen dedup so handleQueued acts on our re-dispatch. Leave
	// attempts intact: handleQueued increments it, so repeated stranding of a
	// genuinely undispatchable job still converges on the zombie cap.
	delete(s.seen, key)
	// Re-dispatch from the scheduler root context, not the exiting
	// wait-goroutine's captured ctx — that ctx may carry a stale retry marker
	// (retryAttemptCtxKey) from the original claim retry, which would misroute
	// a re-claim failure. Fall back to the passed ctx when runCtx is unset
	// (e.g. unit tests that call this without going through Run).
	dispatchCtx := s.runCtx
	s.mu.Unlock()
	if dispatchCtx == nil {
		dispatchCtx = ctx
	}

	s.cfg.Log.Info("dispatched runner exited but its job was never observed running; re-provisioning",
		"job_id", key.JobID,
		"repo", event.Repo,
		"detail", "same-label JIT runners are fungible; the runner likely ran a sibling job and left this one queued")
	go s.handleQueued(dispatchCtx, event)
}

// runReconcileLoop periodically re-runs each provider's catch-up poll while in
// webhook mode. It is now a LAST-RESORT backstop for genuinely dropped webhook
// deliveries — the common fungible-runner stranding case is handled instantly
// and event-drivenly by reprovisionIfStranded on runner exit, so this runs at a
// low frequency (default 30m). The per-job seen/running/zombie/canHandle checks
// in handleQueued still gate every swept job, so there is no double-provision.
func (s *Scheduler) runReconcileLoop(ctx context.Context) {
	var catchers []providers.Provider
	for _, p := range s.cfg.Providers {
		if _, ok := p.(interface{ CatchUpPoll(context.Context) error }); ok {
			catchers = append(catchers, p)
		}
	}
	if len(catchers) == 0 {
		return
	}
	s.cfg.Log.Info("webhook reconcile poll enabled", "interval", s.cfg.ReconcileInterval)
	ticker := time.NewTicker(s.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, p := range catchers {
				catcher := p.(interface{ CatchUpPoll(context.Context) error })
				if err := catcher.CatchUpPoll(ctx); err != nil {
					s.cfg.Log.Warn("reconcile poll failed", "provider", p.Name(), "error", err)
					continue
				}
				s.cfg.Log.Debug("reconcile poll: swept queued jobs", "provider", p.Name())
			}
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

// errCordoned is returned by claimJob when the scheduler is cordoned. It is
// classified errNonRetryable (see classifyErr) so the retry queue drops the
// job rather than spinning its backoff ladder against a node that has been
// told to stop taking work.
var errCordoned = errors.New("scheduler is cordoned; not claiming new jobs")

// claimJob generates a runner name and claims the job via the Provider,
// retrying with a new name if the name already exists (409 conflict).
//
// This is the cordon's LOAD-BEARING gate. Every provisioning path — local,
// Linux-dispatch, macOS-VM — funnels through here, and Provider.ClaimJob is
// the one call that registers a JIT runner with the platform ("registered
// repo-level JIT runner"). admitDispatch is the gate that lets a path unwind
// cleanly, but checking here too is what makes the cordon hold BY
// CONSTRUCTION: a future dispatch source that forgets admitDispatch still
// cannot register a runner on a cordoned node. Issue #154 happened precisely
// because the cordon was checked at one admission point and not at the point
// of claiming.
func (s *Scheduler) claimJob(ctx context.Context, event *providers.JobEvent, labels []string, log *slog.Logger, maxRetries int) (*providers.Claim, error) {
	s.mu.Lock()
	draining := s.draining
	s.mu.Unlock()
	if draining {
		return nil, errCordoned
	}

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

// registerExternalWebhooks registers each webhook-capable provider's hook to
// <ExternalURL>/webhook/<provider> using the configured secret. It is called
// only for the external-tunnel path (tunnel = "external" with external_url set,
// no managed Tunnel) — the managed-tunnel path registers against the tunnel's
// own PublicURL() in Run().
//
// Unlike the managed path this is best-effort: the webhook receiver is already
// serving, so a registration failure is logged as a WARN and skipped rather
// than aborting startup — the operator can still add the hook by hand. The
// underlying provider RegisterWebhooks is idempotent (it reuses an existing
// hook with the same URL), so this is safe to run on every startup. Hooks are
// NOT deregistered on shutdown: external hooks are operator-owned and must
// persist across restarts.
// webhookPathOf returns a function that gives each provider a stable, unique
// webhook path. Normally "/webhook/<name>", but when more than one
// webhook-capable provider shares a name — e.g. one GitHub provider per owner —
// the path is disambiguated by owner ("/webhook/<name>/<owner>") so they never
// collide on the same mux path or webhook URL. Single-provider deployments keep
// the plain "/webhook/<name>" (no path change on upgrade).
func webhookPathOf(all []providers.Provider) func(providers.Provider) string {
	counts := map[string]int{}
	for _, p := range all {
		if _, ok := p.(providers.Webhook); ok {
			counts[p.Name()]++
		}
	}
	return func(p providers.Provider) string {
		base := "/webhook/" + p.Name()
		if counts[p.Name()] > 1 {
			if o, ok := p.(interface{ Owner() string }); ok && o.Owner() != "" {
				return base + "/" + o.Owner()
			}
		}
		return base
	}
}

func (s *Scheduler) registerExternalWebhooks(ctx context.Context, whProviders []providers.Webhook) {
	pathOf := webhookPathOf(s.cfg.Providers)
	for _, whp := range whProviders {
		name := whp.(providers.Provider).Name()
		webhookURL := s.cfg.ExternalURL + pathOf(whp.(providers.Provider))
		s.cfg.Log.Info("registering external webhook", "provider", name, "url", webhookURL)
		if err := whp.RegisterWebhooks(ctx, webhookURL, s.cfg.WebhookSecret); err != nil {
			s.cfg.Log.Warn("failed to register external webhook (continuing; add the hook manually if needed)",
				"provider", name, "url", webhookURL, "error", err)
			continue
		}
		s.cfg.Log.Info("external webhook registered", "provider", name, "url", webhookURL)
	}
}

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
		provs := make([]providers.Provider, len(whProviders))
		for i, w := range whProviders {
			provs[i] = w.(providers.Provider)
		}
		pathOf := webhookPathOf(provs)
		allOK := true
		for _, whp := range whProviders {
			webhookURL := s.cfg.Tunnel.PublicURL() + pathOf(whp.(providers.Provider))
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
