package runnerversion

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/ephpm/ephemerd/pkg/runner"
)

// switchLoader lets a test change what "the network" returns between
// reconciles: a verified manifest, or a fetch failure.
type switchLoader struct {
	mu  sync.Mutex
	m   *Manifest
	err error
}

func (l *switchLoader) Load(context.Context) (*Manifest, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	if l.m == nil {
		return nil, ErrManifestFetch
	}
	return l.m, nil
}

func (l *switchLoader) set(m *Manifest, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m, l.err = m, err
}

// imageTree is a probe result with an unknown owner, so PrepareOverrideTree
// is a no-op (the tests do not run as root and must not chown).
func imageTree(v string) TreeInfo {
	return TreeInfo{Dir: "/home/runner", Version: v, Parts: runner.ParseVersionString(v), UID: -1, GID: -1}
}

type ctlFixture struct {
	loader   *switchLoader
	store    *Store
	sel      *Selector
	ov       *ImageOverride
	det      *Detector
	image    string
	imageVer string
	cfg      ControllerConfig
}

// newCtlFixture: embedded runner `embedded`, official image shipping
// `imageVer`, no fetch dependencies unless the test adds them.
func newCtlFixture(t *testing.T, embedded, imageVer string) *ctlFixture {
	t.Helper()
	withEmbeddedVersion(t, embedded)
	fx := &ctlFixture{
		loader:   &switchLoader{},
		store:    newTestStore(t),
		ov:       NewImageOverride(testLog()),
		det:      &Detector{Threshold: 1, Window: time.Minute, Cooldown: time.Hour, Log: testLog()},
		image:    "ghcr.io/actions/actions-runner:test",
		imageVer: imageVer,
	}
	fx.sel = &Selector{
		Embedded:     runner.New(t.TempDir(), testLog()),
		Store:        fx.store,
		Log:          testLog(),
		HashEmbedded: func() (string, error) { return zeroSHA, nil },
	}
	fx.cfg = ControllerConfig{
		Platform:      "linux-x64",
		Channel:       ChannelStable,
		AutoUpgrade:   true,
		ImageOverride: true,
		Image:         fx.image,
		Manifest:      fx.loader,
		Selector:      fx.sel,
		Override:      fx.ov,
		Store:         fx.store,
		Detector:      fx.det,
		ProbeImage: func(_ context.Context, image string) (TreeInfo, error) {
			if image != fx.image {
				t.Errorf("probed %q, want %q", image, fx.image)
			}
			return imageTree(fx.imageVer), nil
		},
		Log: testLog(),
	}
	return fx
}

func (fx *ctlFixture) controller(t *testing.T) *Controller {
	t.Helper()
	c, err := NewController(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (fx *ctlFixture) tripBreaker() {
	fx.det.Registered("probe-runner", "linux")
	fx.det.Exited("probe-runner", false)
}

func TestNewControllerValidates(t *testing.T) {
	if _, err := NewController(ControllerConfig{}); err == nil {
		t.Fatal("missing Manifest/Selector must fail")
	}
	fx := newCtlFixture(t, "2.337.0", "2.336.0")
	cfg := fx.cfg
	cfg.Override = nil
	if _, err := NewController(cfg); err == nil {
		t.Fatal("Image without Override must fail")
	}
	// The controller installs itself as the detector's hooks.
	c := fx.controller(t)
	if fx.sel.BreakerOpen == nil || fx.det.OnDegraded == nil {
		t.Fatal("controller must wire BreakerOpen and OnDegraded")
	}
	fx.tripBreaker()
	if !c.linuxBreakerOpen() {
		t.Fatal("breaker should read as open")
	}
	select {
	case <-c.kick:
	default:
		t.Fatal("OnDegraded must kick the loop")
	}
}

// The 2026-11-10 shape after a fetch already landed: image below floor,
// cache holds a runner at/above it -> override engaged with the cache
// tree, breaker reset so the next runner is admitted at once, and a
// second pass does not re-engage.
func TestControllerEngagesCacheTreeWhenImageBelowFloor(t *testing.T) {
	fx := newCtlFixture(t, "2.337.0", "2.336.0")
	fx.loader.set(validManifest("2.338.2", "2.338.0", zeroSHA), nil)
	dir := swapIn(t, fx.store, "2.338.2")
	fx.tripBreaker()
	c := fx.controller(t)

	c.Reconcile(context.Background(), "test")
	active, gotDir, ver, image, reason, _ := fx.ov.Status()
	if !active || gotDir != dir || ver != "2.338.2" || image != fx.image || !strings.Contains(reason, "below manifest floor") {
		t.Fatalf("override: active=%v dir=%s ver=%s image=%s reason=%q", active, gotDir, ver, image, reason)
	}
	if st, _ := fx.det.State("linux"); st != BreakerClosed {
		t.Fatalf("breaker should be reset after engaging, got %s", st)
	}
	rep, m, err := c.Report()
	if err != nil || m == nil || rep.Image == nil || !rep.Image.Override || rep.ImageInfo.Version != "2.336.0" {
		t.Fatalf("report: %+v %v %v", rep, m, err)
	}
	// The mounted paths follow the cache too.
	if d, ok := fx.sel.MountedRunnerDir(); !ok || d != dir {
		t.Fatalf("MountedRunnerDir = %q, %v", d, ok)
	}
	// Idempotent.
	c.Reconcile(context.Background(), "poll")
	if active, d2, _, _, _, _ := fx.ov.Status(); !active || d2 != dir {
		t.Fatal("second reconcile must keep the override")
	}
}

// Manifest outage + breaker open engages the embedded runner over the
// image. That state is deliberately sticky while the manifest is missing
// (no flapping on a hunch) and clears the moment a verified manifest says
// the image is healthy — the false-alert case from the #220 review.
func TestControllerNoManifestBreakerPathClearsOnVerifiedManifest(t *testing.T) {
	fx := newCtlFixture(t, "2.337.0", "2.336.0")
	fx.loader.set(nil, ErrManifestFetch)
	c := fx.controller(t)

	// Breaker closed, no manifest: nothing, ever.
	c.Reconcile(context.Background(), "boot")
	if active, _, _, _, _, _ := fx.ov.Status(); active {
		t.Fatal("no manifest + breaker closed must not override")
	}
	// Breaker opens: embedded (newer) goes over the image.
	fx.tripBreaker()
	c.Reconcile(context.Background(), "breaker")
	active, dir, ver, _, reason, _ := fx.ov.Status()
	if !active || dir != fx.sel.Embedded.Dir() || ver != "2.337.0" || !strings.Contains(reason, "breaker open") {
		t.Fatalf("override: %v %s %s %q", active, dir, ver, reason)
	}
	// Engaging reset the breaker; the manifest is still down. Sticky: the
	// override stays rather than flapping off and letting the next three
	// runners die to re-learn the same thing.
	if st, _ := fx.det.State("linux"); st != BreakerClosed {
		t.Fatalf("breaker should have been reset, got %s", st)
	}
	c.Reconcile(context.Background(), "poll")
	if active, _, _, _, _, _ := fx.ov.Status(); !active {
		t.Fatal("override must stay engaged while no manifest can be verified")
	}
	// A verified manifest whose floor the image satisfies clears it.
	fx.loader.set(validManifest("2.338.2", "2.336.0", zeroSHA), nil)
	c.Reconcile(context.Background(), "poll")
	if active, _, _, _, _, _ := fx.ov.Status(); active {
		t.Fatal("verified manifest saying the image is healthy must clear the override")
	}
	// ... and with the breaker open again it is still NOT re-engaged: the
	// manifest is authoritative (the operator raises the floor instead).
	fx.tripBreaker()
	c.Reconcile(context.Background(), "breaker")
	if active, _, _, _, _, _ := fx.ov.Status(); active {
		t.Fatal("a healthy image per the manifest must not be masked on the breaker alone")
	}
}

func TestControllerHealthyImageNeverMasked(t *testing.T) {
	fx := newCtlFixture(t, "2.339.0", "2.338.2")
	fx.loader.set(validManifest("2.338.2", "2.338.0", zeroSHA), nil)
	c := fx.controller(t)
	c.Reconcile(context.Background(), "boot")
	if active, _, _, _, _, _ := fx.ov.Status(); active {
		t.Fatal("image at/above floor must not be masked even with a newer embedded runner")
	}
}

// Nothing on the node is good enough and the fetch has no GitHub source:
// the controller must not engage anything (and must say so in the report).
func TestControllerNeedFetchWithoutFetchDepsEngagesNothing(t *testing.T) {
	fx := newCtlFixture(t, "2.337.0", "2.336.0")
	fx.loader.set(validManifest("2.338.2", "2.338.0", zeroSHA), nil)
	c := fx.controller(t)
	if c.canFetch() {
		t.Fatal("no GitHub source: fetch must be unavailable")
	}
	c.Reconcile(context.Background(), "boot")
	if active, _, _, _, _, _ := fx.ov.Status(); active {
		t.Fatal("must not engage a below-floor tree")
	}
	rep, _, _ := c.Report()
	if rep.Image == nil || !rep.Image.NeedFetch || !rep.BelowFloor {
		t.Fatalf("report: %+v", rep)
	}
	if _, ok := fx.sel.MountedRunnerDir(); ok {
		t.Fatal("no cache entry: mounted paths stay on the embedded tree")
	}
}

// The full break-glass path: both trees below floor -> two-source verified
// fetch -> cache -> override engaged on the fetched tree, and the mounted
// paths redirected to it, all in one reconcile.
func TestControllerFetchesThenEngages(t *testing.T) {
	fx := newCtlFixture(t, "2.337.0", "2.336.0")
	data, sha := fakeArchive(t, "echo fetched")
	srv, client := httpsTestServer(t, serveBytes(t, data, nil))
	m := validManifest("2.338.2", "2.338.0", sha)
	for k, a := range m.Platforms {
		a.URL = srv.URL + "/" + ArchiveFilename(k, "2.338.2")
		m.Platforms[k] = a
	}
	fx.loader.set(m, nil)
	fx.cfg.GitHub = &GitHubSource{list: func(context.Context) ([]*gh.RunnerApplicationDownload, error) {
		return ghDownloads("2.338.2", sha), nil
	}}
	fx.cfg.Fetcher = &Fetcher{Store: fx.store, Client: client, DiskCheck: plentyOfDisk, Log: testLog()}
	c := fx.controller(t)
	if !c.canFetch() {
		t.Fatal("fetch should be available")
	}

	c.Reconcile(context.Background(), "boot")
	cur, ok, err := fx.store.Current()
	if err != nil || !ok || cur.Version != "2.338.2" || cur.SHA256 != sha {
		t.Fatalf("store current: %+v %v %v", cur, ok, err)
	}
	active, dir, ver, _, _, _ := fx.ov.Status()
	if !active || dir != cur.Dir || ver != "2.338.2" {
		t.Fatalf("override after fetch: %v %s %s", active, dir, ver)
	}
	if d, ok := fx.sel.MountedRunnerDir(); !ok || d != cur.Dir {
		t.Fatalf("MountedRunnerDir = %q, %v", d, ok)
	}
	rep, _, _ := c.Report()
	if rep.BelowFloor || rep.Selected.Version != "2.338.2" {
		t.Fatalf("report after fetch: %+v", rep)
	}
	// A second pass with the same manifest: replay-guarded, nothing
	// re-fetched, override untouched.
	c.Reconcile(context.Background(), "poll")
	if active, d2, _, _, _, _ := fx.ov.Status(); !active || d2 != cur.Dir {
		t.Fatal("override must survive a replayed manifest")
	}
}

// Sources disagree (manifest stale vs GitHub): no fetch, nothing engaged.
func TestControllerFetchRefusedWhenSourcesDisagree(t *testing.T) {
	fx := newCtlFixture(t, "2.337.0", "2.336.0")
	data, sha := fakeArchive(t, "echo x")
	srv, client := httpsTestServer(t, serveBytes(t, data, nil))
	m := validManifest("2.338.2", "2.338.0", sha)
	for k, a := range m.Platforms {
		a.URL = srv.URL + "/" + ArchiveFilename(k, "2.338.2")
		m.Platforms[k] = a
	}
	fx.loader.set(m, nil)
	fx.cfg.GitHub = &GitHubSource{list: func(context.Context) ([]*gh.RunnerApplicationDownload, error) {
		return ghDownloads("2.338.3", sha), nil
	}}
	fx.cfg.Fetcher = &Fetcher{Store: fx.store, Client: client, DiskCheck: plentyOfDisk, Log: testLog()}
	c := fx.controller(t)
	c.Reconcile(context.Background(), "boot")
	if _, ok, _ := fx.store.Current(); ok {
		t.Fatal("disagreeing sources must not populate the store")
	}
	if active, _, _, _, _, _ := fx.ov.Status(); active {
		t.Fatal("nothing verified: nothing engaged")
	}
}

func TestControllerImageOverrideDisabledOnlyReports(t *testing.T) {
	fx := newCtlFixture(t, "2.338.2", "2.336.0")
	fx.loader.set(validManifest("2.338.2", "2.338.0", zeroSHA), nil)
	fx.cfg.ImageOverride = false
	c := fx.controller(t)
	c.Reconcile(context.Background(), "boot")
	if active, _, _, _, _, _ := fx.ov.Status(); active {
		t.Fatal("image_override = false must never engage")
	}
	rep, _, _ := c.Report()
	if rep.Image == nil || !rep.Image.Override {
		t.Fatalf("the decision is still reported: %+v", rep.Image)
	}
}

func TestControllerIgnoresOtherChannelAndKeepsLastManifest(t *testing.T) {
	fx := newCtlFixture(t, "2.337.0", "2.336.0")
	stable := validManifest("2.338.2", "2.338.0", zeroSHA)
	fx.loader.set(stable, nil)
	c := fx.controller(t)
	c.Reconcile(context.Background(), "boot")
	if _, m, _ := c.Report(); m == nil || m.Channel != ChannelStable {
		t.Fatal("stable manifest should be loaded")
	}
	// A canary manifest (or a fetch failure) does not displace it.
	canary := validManifest("2.339.0", "2.339.0", zeroSHA)
	canary.Channel = ChannelCanary
	fx.loader.set(canary, nil)
	c.Reconcile(context.Background(), "poll")
	if _, m, _ := c.Report(); m == nil || m.Version != "2.338.2" {
		t.Fatalf("canary manifest must be ignored on a stable node, got %+v", m)
	}
	fx.loader.set(nil, errors.New("boom"))
	c.Reconcile(context.Background(), "poll")
	if _, m, _ := c.Report(); m == nil || m.Version != "2.338.2" {
		t.Fatalf("fetch failure must keep the last verified manifest, got %+v", m)
	}
}

// A manifest that lists the embedded version with a DIFFERENT hash is a
// supply-chain signal: boot-verify fails and nothing is engaged on top.
func TestControllerTamperedEmbeddedEngagesNothing(t *testing.T) {
	fx := newCtlFixture(t, "2.338.2", "2.336.0")
	other := strings.Repeat("ab", 32)
	fx.loader.set(validManifest("2.338.2", "2.338.0", other), nil)
	c := fx.controller(t)
	c.Reconcile(context.Background(), "boot")
	if _, _, err := c.Report(); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("expected tamper error, got %v", err)
	}
	if active, _, _, _, _, _ := fx.ov.Status(); active {
		t.Fatal("tampered embed must not be mounted anywhere")
	}
}

// Run: boot reconcile happens, a kick triggers another, ctx stops it.
func TestControllerRunHonoursKickAndContext(t *testing.T) {
	fx := newCtlFixture(t, "2.337.0", "2.336.0")
	fx.loader.set(validManifest("2.338.2", "2.338.0", zeroSHA), nil)
	var probes int
	inner := fx.cfg.ProbeImage
	fx.cfg.ProbeImage = func(ctx context.Context, image string) (TreeInfo, error) {
		probes++
		return inner(ctx, image)
	}
	fx.cfg.PollInterval = time.Hour
	swept := 0
	fx.cfg.SweepProbes = func(context.Context) int { swept++; return 0 }
	c := fx.controller(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, m, _ := c.Report(); m != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("boot reconcile did not happen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.Kick("test")
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := probes
		c.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on ctx cancel")
	}
	if swept != 1 || probes < 2 {
		t.Fatalf("swept=%d probes=%d", swept, probes)
	}
}
