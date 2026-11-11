package main

import (
	"context"
	"fmt"
	"log/slog"
	runtime_ "runtime"

	gh "github.com/google/go-github/v72/github"

	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/providers"
	githubProv "github.com/ephpm/ephemerd/pkg/providers/github"
	"github.com/ephpm/ephemerd/pkg/runner"
	"github.com/ephpm/ephemerd/pkg/runnerversion"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"github.com/ephpm/ephemerd/pkg/scheduler"
)

// Self-healing runner version: the daemon-side assembly
// (docs/arch/self-healing-runner.md). ON by default; [runner.self_heal]
// enabled = false is the kill-switch and leaves every seam below nil, which
// is byte-for-byte the pre-0.3.0 daemon.
//
// Two daemons run this: the host scheduler (every host) and the in-VM
// worker (--containerd-only on Windows/macOS hosts, where the Linux jobs
// and therefore the official-image path live). Only the host has a
// scheduler, so only the host runs the zero-work detector; the worker runs
// boot-verify, the image probe/override and — when the staged config
// carries a GitHub token — the break-glass fetch.

// selfHeal holds the pieces that must exist before the runtime is built
// (the runtime consults runnerSource on every Create) and the ones the
// controller needs once the runtime and providers exist.
type selfHeal struct {
	cfg      config.SelfHealToml
	log      *slog.Logger
	platform string
	key      *runnerversion.PublicKey
	store    *runnerversion.Store
	fetcher  *runnerversion.Fetcher
	selector *runnerversion.Selector
	override *runnerversion.ImageOverride
	detector *runnerversion.Detector
	source   *runnerSource
}

// runnerSource is what the runtime consults on every Create: the
// official-image override (pkg/runtime.RunnerOverrideSource) and the
// verified-cache redirect for the mounted paths
// (pkg/runtime.MountedRunnerSource), in one value.
type runnerSource struct {
	*runnerversion.ImageOverride
	*runnerversion.Selector
}

// newSelfHeal builds the subsystem's state for this daemon, or returns nil
// when the kill-switch is set. withDetector is true for the host scheduler
// daemon. An unparsable public_key override is a config error and fails
// the daemon fast.
func newSelfHeal(cfg *config.Config, dataDir string, rm *runner.Manager, withDetector bool, log *slog.Logger) (*selfHeal, error) {
	sh := cfg.Runner.SelfHeal
	if !sh.ResolvedEnabled() {
		log.Warn("runner self-heal DISABLED by [runner.self_heal] enabled = false; a stale runner will not be detected or replaced on this node")
		return nil, nil
	}
	log = log.With("component", "runner-self-heal")
	var key *runnerversion.PublicKey
	var err error
	if sh.PublicKey != "" {
		key, err = runnerversion.ParsePublicKey(sh.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("[runner.self_heal] public_key: %w", err)
		}
		log.Warn("runner manifest public key overridden by config; the compiled-in trust root is not used", "key_id", fmt.Sprintf("%X", key.KeyID))
	} else {
		key, err = runnerversion.EmbeddedPublicKey()
		if err != nil {
			return nil, fmt.Errorf("embedded runner-manifest public key: %w (this is a build defect)", err)
		}
	}
	store := runnerversion.NewStore(dataDir, log)
	s := &selfHeal{
		cfg:      sh,
		log:      log,
		platform: runnerversion.PlatformKey(runtime_.GOOS, runtime_.GOARCH),
		key:      key,
		store:    store,
		fetcher: &runnerversion.Fetcher{
			Store:        store,
			MaxBytes:     sh.ResolvedMaxArchiveBytes(),
			MinFreeBytes: sh.ResolvedMinFreeBytes(),
			Log:          log,
		},
		selector: &runnerversion.Selector{Embedded: rm, Store: store, Log: log},
		override: runnerversion.NewImageOverride(log),
	}
	if withDetector {
		s.detector = &runnerversion.Detector{
			Window:    sh.ResolvedZeroWorkWindow(),
			Threshold: sh.ResolvedZeroWorkThreshold(),
			Cooldown:  sh.ResolvedBreakerCooldown(),
			Log:       log,
		}
	}
	s.source = &runnerSource{ImageOverride: s.override, Selector: s.selector}
	return s, nil
}

// runnerOverride is the value for runtime.Config.RunnerOverride: a typed
// nil must not become a non-nil interface, so the nil case is explicit.
func (s *selfHeal) runnerOverride() runtime.RunnerOverrideSource {
	if s == nil {
		return nil
	}
	return s.source
}

// runnerHealth is the value for scheduler.Config.RunnerHealth.
func (s *selfHeal) runnerHealth() scheduler.RunnerHealth {
	if s == nil || s.detector == nil {
		return nil
	}
	return detectorHooks{s.detector}
}

// detectorHooks adapts the Detector to scheduler.RunnerHealth (the
// Detector's Exited returns a Verdict the scheduler has no use for).
type detectorHooks struct{ d *runnerversion.Detector }

func (h detectorHooks) Registered(name, platform string) { h.d.Registered(name, platform) }
func (h detectorHooks) JobStarted(name string)           { h.d.JobStarted(name) }
func (h detectorHooks) Exited(name string, killedByUs bool) {
	h.d.Exited(name, killedByUs)
}
func (h detectorHooks) Allow(platform string) bool { return h.d.Allow(platform) }

// start builds the controller and runs it in the background. image is the
// official Linux image this daemon's runtime runs jobs on ("" when this
// daemon has no official-image path); ghSrc is the GitHub runners/downloads
// source for the two-source fetch, or nil when no GitHub credential is
// available to this daemon.
func (s *selfHeal) start(ctx context.Context, rt *runtime.Runtime, image string, ghSrc runnerversion.Source) error {
	if s == nil {
		return nil
	}
	if image != "" && !runtime.IsOfficialRunnerImage(image) {
		s.log.Info("default Linux image is not an official runner image; it mounts the configured runner, so there is no image runner to probe", "image", image)
		image = ""
	}
	ctl, err := runnerversion.NewController(runnerversion.ControllerConfig{
		Platform:      s.platform,
		Channel:       runnerversion.Channel(s.cfg.ResolvedChannel()),
		PollInterval:  s.cfg.ResolvedPollInterval(),
		AutoUpgrade:   s.cfg.ResolvedAutoUpgrade(),
		ImageOverride: s.cfg.ResolvedImageOverride(),
		Image:         image,
		Manifest:      &runnerversion.HTTPManifestLoader{URL: s.cfg.ResolvedManifestURL(), Key: s.key},
		Selector:      s.selector,
		Override:      s.override,
		Store:         s.store,
		Fetcher:       s.fetcher,
		GitHub:        ghSrc,
		Detector:      s.detector,
		ProbeImage:    rt.ProbeImageRunner,
		SweepProbes:   rt.SweepRunnerProbeViews,
		Log:           s.log,
	})
	if err != nil {
		return fmt.Errorf("runner self-heal: %w", err)
	}
	go ctl.Run(ctx)
	return nil
}

// linuxJobImage is the official Linux image this daemon's OWN runtime
// runs Linux jobs on, or "" when this daemon does not run Linux containers
// itself (Windows/macOS hosts dispatch them into the VM worker, which runs
// its own self-heal).
func linuxJobImage(cfg *config.Config, containerdOnly bool) string {
	if runtime_.GOOS != "linux" && !containerdOnly {
		return ""
	}
	if cfg.Runner.DefaultImage != "" {
		return cfg.Runner.DefaultImage
	}
	return runtime.DefaultLinuxImage()
}

// githubRunnerSource picks the first GitHub provider's authenticated
// client for the runners/downloads cross-check, scoped to its first repo
// when it has one (the repo endpoint needs only the repo-admin permission
// JIT registration already uses; the org endpoint needs admin:org).
func githubRunnerSource(active []providers.Provider) runnerversion.Source {
	for _, p := range active {
		gp, ok := p.(*githubProv.Provider)
		if !ok || gp.Client() == nil {
			continue
		}
		c := gp.Client()
		src := &runnerversion.GitHubSource{Client: c.RESTClient(), Owner: c.Owner()}
		if repos := c.Repos(); len(repos) > 0 {
			src.Repo = repos[0]
		}
		return src
	}
	return nil
}

// workerGitHubRunnerSource is the worker-daemon equivalent: the staged
// host config may carry a PAT (Windows hosts stage config.toml into the
// VM); an App credential needs a key file the VM does not have, so only
// the token form is used. Nil disables the fetch in the VM, leaving the
// override to the embedded runner.
func workerGitHubRunnerSource(cfg *config.Config) runnerversion.Source {
	for _, t := range cfg.GitHubTargets() {
		if t.Token == "" || t.AppID != 0 {
			continue
		}
		src := &runnerversion.GitHubSource{Client: gh.NewClient(nil).WithAuthToken(t.Token), Owner: t.Owner}
		if len(t.Repos) > 0 {
			src.Repo = t.Repos[0]
		}
		return src
	}
	return nil
}
