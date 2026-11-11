package runnerversion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/metrics"
)

// Controller is the daemon-side driver of the subsystem: it loads the
// signed manifest, runs boot-verify at start and on every poll, turns the
// image decision into Engage/Clear/fetch-then-Engage, and reacts to the
// zero-work breaker. It is the only thing in this package that holds
// policy; the pieces it drives (Selector, Upgrader, Detector,
// ImageOverride) stay independently testable.
//
// Lifecycle: NewController, then Run(ctx) in its own goroutine. Run never
// returns an error — the subsystem is a safety net, and every failure
// inside it degrades to "the node runs what it has today" with a log line
// and a metric, never to a stopped daemon.
//
// Linux platform key for the breaker/image path. The official-image path
// exists only for Linux jobs, whatever host they are dispatched from.
const linuxPlatform = "linux"

// ControllerConfig wires the controller.
type ControllerConfig struct {
	// Platform is PlatformKey(GOOS, GOARCH) of THIS daemon's runner — the
	// one the mounted paths serve and the one the break-glass fetch
	// downloads.
	Platform string
	// Channel is the node's manifest channel ("" = accept any).
	Channel Channel
	// PollInterval is the manifest re-check cadence. <= 0 = 1h.
	PollInterval time.Duration
	// AutoUpgrade enables the break-glass fetch ([runner.self_heal]
	// auto_upgrade).
	AutoUpgrade bool
	// ImageOverride enables masking a stale official-image runner
	// ([runner.self_heal] image_override).
	ImageOverride bool
	// Image is the official Linux runner image this node runs jobs on;
	// "" = this daemon has no official-image path (Windows/macOS hosts).
	Image string

	Manifest ManifestLoader // required
	Selector *Selector      // required
	Override *ImageOverride // required when Image != ""
	// Store/Fetcher/GitHub are the fetch dependencies; any nil disables
	// the break-glass fetch (logged once at Run).
	Store   *Store
	Fetcher *Fetcher
	GitHub  Source
	// Detector is the zero-work detector (nil on a worker daemon that has
	// no scheduler). The controller installs itself as its OnDegraded.
	Detector *Detector
	// ProbeImage reads the runner baked into Image (runtime.ProbeImageRunner).
	ProbeImage func(ctx context.Context, image string) (TreeInfo, error)
	// SweepProbes removes probe views leaked by a previous boot
	// (runtime.SweepRunnerProbeViews). Optional.
	SweepProbes func(ctx context.Context) int

	Log *slog.Logger
	Now func() time.Time
}

// Controller drives the subsystem. See the package comment on this file.
type Controller struct {
	cfg      ControllerConfig
	upgrader *Upgrader
	log      *slog.Logger
	now      func() time.Time

	// kick wakes Run for an out-of-band reconcile (breaker opened).
	kick chan string

	mu       sync.Mutex // serializes reconcile and guards the fields below
	manifest *Manifest  // last VERIFIED manifest, kept across fetch failures
	report   BootReport // last reconcile's report
	lastErr  error
}

// NewController validates the wiring and returns a controller that has
// not run yet.
func NewController(cfg ControllerConfig) (*Controller, error) {
	if cfg.Manifest == nil || cfg.Selector == nil {
		return nil, errors.New("runnerversion controller: Manifest and Selector are required")
	}
	if cfg.Image != "" && (cfg.Override == nil || cfg.ProbeImage == nil) {
		return nil, errors.New("runnerversion controller: Image set but Override/ProbeImage missing")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Hour
	}
	c := &Controller{cfg: cfg, log: cfg.Log.With("component", "runner-self-heal"), now: cfg.Now, kick: make(chan string, 1)}
	if c.canFetch() {
		c.upgrader = &Upgrader{
			AutoUpgrade: true,
			Channel:     cfg.Channel,
			Platform:    cfg.Platform,
			Manifest:    cfg.Manifest,
			GitHub:      cfg.GitHub,
			Fetcher:     cfg.Fetcher,
			Store:       cfg.Store,
			Log:         c.log,
			Now:         cfg.Now,
		}
	}
	if cfg.Detector != nil {
		cfg.Selector.BreakerOpen = c.linuxBreakerOpen
		if cfg.Detector.OnDegraded == nil {
			cfg.Detector.OnDegraded = c.OnDegraded
		}
	}
	return c, nil
}

func (c *Controller) canFetch() bool {
	return c.cfg.AutoUpgrade && c.cfg.Store != nil && c.cfg.Fetcher != nil && c.cfg.GitHub != nil
}

func (c *Controller) linuxBreakerOpen() bool {
	if c.cfg.Detector == nil {
		return false
	}
	st, _ := c.cfg.Detector.State(linuxPlatform)
	return st != BreakerClosed
}

// OnDegraded is the Detector's CLOSED→OPEN hook: wake the loop so the
// upgrade/override decision runs now rather than at the next poll.
func (c *Controller) OnDegraded(platform string, streak int, reason string) {
	c.Kick(fmt.Sprintf("breaker opened on %s after %d %s runners", platform, streak, reason))
}

// Kick requests an out-of-band reconcile. Non-blocking; coalesces.
func (c *Controller) Kick(reason string) {
	select {
	case c.kick <- reason:
	default:
	}
}

// Run performs the boot reconcile and then polls until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	c.log.Info("runner self-heal active",
		"platform", c.cfg.Platform, "channel", string(c.cfg.Channel), "poll", c.cfg.PollInterval.String(),
		"image", c.cfg.Image, "image_override", c.cfg.ImageOverride, "auto_upgrade", c.cfg.AutoUpgrade,
		"fetch_available", c.canFetch(), "detector", c.cfg.Detector != nil)
	if c.cfg.AutoUpgrade && !c.canFetch() {
		c.log.Warn("break-glass fetch unavailable on this daemon: no GitHub credential to cross-check runners/downloads",
			"detail", "the override can still mount the embedded runner over a stale image; a fetch needs [github] auth")
	}
	if c.cfg.SweepProbes != nil {
		c.cfg.SweepProbes(ctx)
	}
	if c.cfg.Store != nil {
		if err := c.cfg.Store.Init(); err != nil {
			c.log.Warn("initializing verified runner store", "error", err)
		} else if err := c.cfg.Store.Prune(); err != nil {
			c.log.Warn("pruning verified runner store at boot", "error", err)
		}
	}
	c.Reconcile(ctx, "boot")
	t := time.NewTicker(c.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Reconcile(ctx, "poll")
		case why := <-c.kick:
			c.Reconcile(ctx, why)
		}
	}
}

// Report returns the last reconcile's outcome (for status/healthz).
func (c *Controller) Report() (BootReport, *Manifest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.report, c.manifest, c.lastErr
}

// Reconcile runs one verify-decide-act pass. Exported for tests and for
// an operator-triggered re-check; Run calls it at boot, on every poll and
// on a breaker kick.
func (c *Controller) Reconcile(ctx context.Context, trigger string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	log := c.log.With("trigger", trigger)

	m := c.loadManifestLocked(ctx, log)

	if c.cfg.Image != "" {
		image := c.cfg.Image
		c.cfg.Selector.ProbeImage = func() (TreeInfo, error) {
			pctx, cancel := context.WithTimeout(ctx, 10*time.Minute) // may pull the image on first boot
			defer cancel()
			return c.cfg.ProbeImage(pctx, image)
		}
	}
	rep, err := c.cfg.Selector.BootVerify(m, c.cfg.Platform)
	c.report, c.lastErr = rep, err
	if err != nil {
		// Only EmbeddedTampered or an unreadable embed reach here: a build
		// or supply-chain problem. Nothing is engaged on top of it.
		metrics.RunnerManifestErrors.WithLabelValues("embedded").Inc()
		log.Error("runner boot-verify failed; self-heal will not act on this daemon until it passes", "error", err)
		return
	}
	floor := ""
	if m != nil {
		floor = m.Floor
	}
	log.Info("runner boot-verify",
		"selected", rep.Selected.Version, "source", rep.Selected.Source, "manifest_floor", floor,
		"below_floor", rep.BelowFloor, "embedded_matches_manifest", rep.EmbeddedMatchesManifest,
		"image_runner", rep.ImageInfo.Version, "image_probe_error", rep.ImageErr)

	// Mounted paths (custom images, Windows): the selected runner is what
	// those jobs mount. Below the floor it must be replaced; the selector
	// redirects the next job at the cache the moment a swap lands.
	if rep.BelowFloor {
		if res, uerr := c.upgradeLocked(ctx, log, "selected runner "+rep.Selected.Version+" is below manifest floor "+floor); uerr == nil && res.Swapped {
			rep.Selected = c.cfg.Selector.Select()
			rep.BelowFloor = m.BelowFloor(rep.Selected.Parts)
			c.report = rep
		} else if uerr != nil {
			log.Error("runner below manifest floor and the break-glass fetch did not resolve it: jobs on the mounted runner paths will be refused by GitHub",
				"selected", rep.Selected.Version, "floor", floor, "error", uerr,
				"action", "merge a release that pins a current actions/runner; or set [runner.self_heal] auto_upgrade = true with [github] auth so the node can fetch one")
		}
	}

	// The official-image path (Linux jobs).
	if c.cfg.Image == "" {
		return
	}
	metrics.SetRunnerImageVersion(c.cfg.Image, rep.ImageInfo.Version)
	if rep.ImageErr != nil {
		log.Warn("official image runner probe failed; the image is treated as unknown and never masked on a guess",
			"image", c.cfg.Image, "error", rep.ImageErr)
		return
	}
	if rep.Image == nil {
		return
	}
	c.applyImageDecisionLocked(ctx, log, m, &rep)
	c.report = rep
}

// applyImageDecisionLocked turns the decision into Engage / Clear / fetch.
func (c *Controller) applyImageDecisionLocked(ctx context.Context, log *slog.Logger, m *Manifest, rep *BootReport) {
	d := *rep.Image
	image := c.cfg.Image

	// Nothing on the node is good enough: fetch, then re-decide with the
	// same probe (no second mount) and the fresh selection.
	if d.NeedFetch && c.canFetch() {
		if res, err := c.upgradeLocked(ctx, log, d.Reason); err == nil && (res.Swapped || res.Dir != "") {
			rep.Selected = c.cfg.Selector.Select()
			d = DecideImageOverride(rep.ImageInfo, rep.Selected, m, c.linuxBreakerOpen())
			rep.Image = &d
		}
	}

	active, curDir, curVer, _, _, _ := c.cfg.Override.Status()
	switch {
	case d.Override:
		if !c.cfg.ImageOverride {
			log.Error("official image runner is stale but [runner.self_heal] image_override = false: Linux jobs will be refused by GitHub until the image is rebuilt",
				"image", image, "image_runner", d.ImageVersion, "available", d.AvailableVersion, "reason", d.Reason)
			return
		}
		if active && curDir == rep.Selected.Dir && curVer == rep.Selected.Version {
			return // already engaged on this tree; do not re-alert
		}
		if err := PrepareOverrideTree(rep.Selected.Dir, rep.ImageInfo); err != nil {
			log.Error("refusing to engage runner image override: could not prepare the override tree", "error", err, "dir", rep.Selected.Dir)
			return
		}
		c.cfg.Override.Engage(rep.Selected.Dir, rep.Selected.Version, image, d.Reason)
		if c.cfg.Detector != nil {
			// The next runner mounts a current tree; admit it now rather
			// than after the breaker cooldown.
			c.cfg.Detector.Reset(linuxPlatform)
		}
	case d.NeedFetch:
		log.Error("official image runner is stale and no verified runner on this node is newer: break-glass fetch unavailable or failed",
			"image", image, "image_runner", d.ImageVersion, "available", d.AvailableVersion,
			"auto_upgrade", c.cfg.AutoUpgrade, "fetch_available", c.canFetch(), "reason", d.Reason,
			"action", "rebuild and push the runner-ci-linux image with a current actions/runner; enable auto_upgrade with [github] auth for the node to bridge it")
	default:
		// Not stale, or unknown. A VERIFIED manifest that says the image is
		// at/above the floor is authoritative: it clears an override that
		// was engaged earlier (including one engaged on the breaker alone
		// during a manifest outage — that path is otherwise sticky and
		// would alert "IMAGE RUNNER IS STALE" forever). With NO manifest an
		// engaged override is left alone: never clear on a hunch, or a
		// webhook outage would flap the override every poll.
		if m == nil || d.ImageVersion == "" {
			return
		}
		if active {
			c.cfg.Override.Clear(fmt.Sprintf("verified manifest (floor %s) says image runner %s is not stale", m.Floor, d.ImageVersion))
		}
		if c.linuxBreakerOpen() {
			log.Warn("linux runner breaker is open but the signed manifest says the image runner is at/above the floor; not masking a healthy image",
				"image", image, "image_runner", d.ImageVersion, "floor", m.Floor,
				"hint", "if GitHub raised its minimum runner version, republish the manifest with a higher floor (Runner manifest workflow, floor input); otherwise check webhook delivery and GitHub status")
		}
	}
}

// upgradeLocked runs one break-glass attempt. ErrDisabled / missing deps
// never reach here (canFetch gates it); a replayed manifest means there is
// nothing newer to apply and is not an error for the caller.
func (c *Controller) upgradeLocked(ctx context.Context, log *slog.Logger, why string) (Result, error) {
	if c.upgrader == nil {
		return Result{}, ErrDisabled
	}
	log.Warn("runner break-glass fetch starting", "reason", why)
	uctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	res, err := c.upgrader.Run(uctx)
	switch {
	case err == nil:
		log.Info("runner break-glass fetch finished", "version", res.Version, "swapped", res.Swapped, "reason", res.Reason)
		if res.Swapped && c.cfg.Detector != nil {
			c.cfg.Detector.Reset(c.hostPlatformClass())
		}
		return res, nil
	case errors.Is(err, ErrManifestReplayed):
		log.Info("runner break-glass fetch: manifest already applied; nothing newer to fetch", "error", err)
		cur, ok, cerr := c.cfg.Store.Current()
		if cerr == nil && ok {
			return Result{Version: cur.Version, SHA256: cur.SHA256, Dir: cur.Dir, Reason: "already applied"}, nil
		}
		return Result{}, err
	default:
		log.Error("runner break-glass fetch failed", "error", err)
		return Result{}, err
	}
}

// hostPlatformClass maps this daemon's runner platform to the breaker's
// dispatch class for the MOUNTED paths it serves.
func (c *Controller) hostPlatformClass() string {
	switch {
	case len(c.cfg.Platform) >= 4 && c.cfg.Platform[:4] == "win-":
		return "windows"
	case len(c.cfg.Platform) >= 4 && c.cfg.Platform[:4] == "osx-":
		return "macos"
	default:
		return linuxPlatform
	}
}

// loadManifestLocked fetches and verifies the manifest. On any failure it
// returns the last verified manifest (or nil), after counting the error:
// a transport failure is a warning, a signature/schema failure an error.
func (c *Controller) loadManifestLocked(ctx context.Context, log *slog.Logger) *Manifest {
	lctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	m, err := c.cfg.Manifest.Load(lctx)
	if err != nil {
		reason := manifestErrorReason(err)
		metrics.RunnerManifestErrors.WithLabelValues(reason).Inc()
		msg := "runner manifest unavailable; running on the last verified manifest"
		if c.manifest == nil {
			msg = "runner manifest unavailable; no verified manifest yet (floor unknown)"
		}
		if reason == "fetch" {
			log.Warn(msg, "error", err)
		} else {
			log.Error("runner manifest REJECTED (signature or schema); ignoring it", "error", err, "reason", reason)
		}
		return c.manifest
	}
	if c.cfg.Channel != "" && m.Channel != c.cfg.Channel {
		log.Warn("runner manifest is for another channel; ignoring it", "manifest_channel", string(m.Channel), "node_channel", string(c.cfg.Channel))
		return c.manifest
	}
	if c.manifest == nil || !m.PublishedAt.Equal(c.manifest.PublishedAt) {
		log.Info("runner manifest loaded", "version", m.Version, "floor", m.Floor, "channel", string(m.Channel),
			"published_at", m.PublishedAt.Format(time.RFC3339), "rollback", m.Rollback)
	}
	c.manifest = m
	return m
}
