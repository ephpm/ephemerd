package runnerversion

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/runner"
)

// ghDownloadsAt is ghDownloads with download URLs under base (a test
// server) instead of github.com.
func ghDownloadsAt(base, version, sha string) []*gh.RunnerApplicationDownload {
	out := ghDownloads(version, sha)
	for _, d := range out {
		d.DownloadURL = gh.Ptr(base + "/" + d.GetFilename())
	}
	return out
}

// breakGlassFixture: the signed manifest says 2.338.2; GitHub can be made
// to say anything; both archives are served by one test server keyed by
// filename, with hit counters so a refusal is provably network-free.
type breakGlassFixture struct {
	u            *Upgrader
	store        *Store
	manifest     *Manifest
	mSHA, gSHA   string
	mHits, gHits atomic.Int32
	breakerOpen  atomic.Bool
	now          time.Time
	ghVersion    string
	ghSHA        string
}

func newBreakGlassFixture(t *testing.T) *breakGlassFixture {
	t.Helper()
	withEmbeddedVersion(t, "2.337.0")
	fx := &breakGlassFixture{now: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	mData, mSHA := fakeArchive(t, "echo manifest 2.338.2")
	gData, gSHA := fakeArchive(t, "echo github 2.339.0")
	fx.mSHA, fx.gSHA = mSHA, gSHA
	mux := http.NewServeMux()
	mux.Handle("/"+ArchiveFilename("linux-x64", "2.338.2"), serveBytes(t, mData, &fx.mHits))
	mux.Handle("/"+ArchiveFilename("linux-x64", "2.339.0"), serveBytes(t, gData, &fx.gHits))
	srv, client := httpsTestServer(t, mux)

	fx.manifest = validManifest("2.338.2", "2.338.0", mSHA)
	for k, a := range fx.manifest.Platforms {
		a.URL = srv.URL + "/" + ArchiveFilename(k, "2.338.2")
		fx.manifest.Platforms[k] = a
	}
	fx.ghVersion, fx.ghSHA = "2.339.0", gSHA
	fx.store = newTestStore(t)
	fx.u = &Upgrader{
		AutoUpgrade: true,
		Channel:     ChannelStable,
		Platform:    "linux-x64",
		Manifest:    StaticManifestLoader{Manifest: fx.manifest},
		GitHub: &GitHubSource{list: func(context.Context) ([]*gh.RunnerApplicationDownload, error) {
			return ghDownloadsAt(srv.URL, fx.ghVersion, fx.ghSHA), nil
		}},
		Fetcher:            &Fetcher{Store: fx.store, Client: client, DiskCheck: plentyOfDisk, Log: testLog()},
		Store:              fx.store,
		Log:                testLog(),
		Now:                func() time.Time { return fx.now },
		BreakGlassOpen:     func() bool { return fx.breakerOpen.Load() },
		BreakGlassCooldown: 15 * time.Minute,
	}
	return fx
}

func (fx *breakGlassFixture) run(t *testing.T) (Result, error) {
	t.Helper()
	return fx.u.Run(context.Background())
}

func breakGlassGauge(t *testing.T) float64 {
	t.Helper()
	return testutil.ToFloat64(metrics.RunnerBreakGlassActive)
}

func versionInfo(t *testing.T, version, source string) float64 {
	t.Helper()
	return testutil.ToFloat64(metrics.RunnerVersionInfo.WithLabelValues(version, source))
}

// (a) breaker OPEN + GitHub strictly ahead of the manifest -> break-glass
// fetch on GitHub's checksum alone; (d) the audit trail: sidecar lists
// GitHub as the ONLY source, version_info carries source=github-breakglass,
// the alert gauge is 1, and the selector labels it break-glass.
func TestBreakGlass_BreakerOpenAndGitHubAheadFetches(t *testing.T) {
	fx := newBreakGlassFixture(t)
	fx.breakerOpen.Store(true)

	res, err := fx.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Swapped || !res.BreakGlass || res.Version != "2.339.0" || res.SHA256 != fx.gSHA || !strings.Contains(res.Dir, "2.339.0-linux") {
		t.Fatalf("result %+v", res)
	}
	if fx.gHits.Load() != 1 || fx.mHits.Load() != 0 {
		t.Fatalf("fetched github=%d manifest=%d archives, want 1/0", fx.gHits.Load(), fx.mHits.Load())
	}
	cur, ok, _ := fx.store.Current()
	if !ok || cur.Version != "2.339.0" || cur.SHA256 != fx.gSHA {
		t.Fatalf("current %+v %v", cur, ok)
	}
	sc, err := fx.store.Sidecar(fx.gSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Sources) != 1 || sc.Sources[0] != SourceGitHubDownloads {
		t.Fatalf("break-glass sidecar must list GitHub only, got %v", sc.Sources)
	}
	// The manifest was NOT applied: a later re-signed one is not a replay.
	if last, _ := fx.store.LastApplied(); !last.IsZero() {
		t.Fatalf("break-glass must not record the manifest as applied, got %v", last)
	}
	if breakGlassGauge(t) != 1 || versionInfo(t, "2.339.0", metrics.RunnerSourceGitHubBreakGlass) != 1 {
		t.Fatal("alert metrics not set")
	}
	sel := (&Selector{Embedded: runner.New(t.TempDir(), testLog()), Store: fx.store, Log: testLog()}).Select()
	if !sel.BreakGlass || sel.Source != metrics.RunnerSourceGitHubBreakGlass || sel.Version != "2.339.0" {
		t.Fatalf("selector must label the entry break-glass: %+v", sel)
	}
}

// (b) sources disagree but the breaker is CLOSED: refuse, no network, no
// store change. Steady state is never loosened.
func TestBreakGlass_BreakerClosedRefusesDisagreement(t *testing.T) {
	fx := newBreakGlassFixture(t)
	fx.breakerOpen.Store(false)
	_, err := fx.run(t)
	if !errors.Is(err, ErrSourcesDisagree) {
		t.Fatalf("want ErrSourcesDisagree, got %v", err)
	}
	if fx.gHits.Load() != 0 || fx.mHits.Load() != 0 {
		t.Fatal("refusal must not touch the network")
	}
	if _, ok, _ := fx.store.Current(); ok {
		t.Fatal("refusal must not change the store")
	}
	// No BreakGlassOpen hook at all (worker daemon): same refusal.
	fx.u.BreakGlassOpen = nil
	if _, err := fx.run(t); !errors.Is(err, ErrSourcesDisagree) {
		t.Fatalf("nil hook: want ErrSourcesDisagree, got %v", err)
	}
}

// (c) roll-forward only: GitHub EQUAL to the manifest (different hash) or
// BEHIND it is refused even with the breaker open. Also refused: GitHub
// ahead of the manifest but not ahead of what the node already has.
func TestBreakGlass_RollForwardOnly(t *testing.T) {
	cases := []struct {
		name      string
		ghVersion string
		prime     func(fx *breakGlassFixture, t *testing.T)
	}{
		{"github equal, hash differs", "2.338.2", nil},
		{"github behind the pin", "2.338.1", nil},
		{"github behind the embedded runner", "2.336.0", nil},
		{"github ahead of manifest but not of the cache", "2.339.0", func(fx *breakGlassFixture, t *testing.T) {
			swapIn(t, fx.store, "2.339.0") // the node already holds 2.339.0
		}},
		{"github ahead of manifest but not of the embedded runner", "2.339.0", func(fx *breakGlassFixture, t *testing.T) {
			withEmbeddedVersion(t, "2.339.0")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newBreakGlassFixture(t)
			fx.breakerOpen.Store(true)
			fx.ghVersion = tc.ghVersion
			if tc.prime != nil {
				tc.prime(fx, t)
			}
			before := fx.gHits.Load() + fx.mHits.Load()
			_, err := fx.run(t)
			if !errors.Is(err, ErrSourcesDisagree) {
				t.Fatalf("want ErrSourcesDisagree, got %v", err)
			}
			if fx.gHits.Load()+fx.mHits.Load() != before {
				t.Fatal("refusal must not download anything")
			}
		})
	}
}

// One shot per cooldown: a second attempt inside the cooldown is refused
// without a download; after the cooldown the gate is evaluated again.
func TestBreakGlass_OneShotPerCooldown(t *testing.T) {
	fx := newBreakGlassFixture(t)
	fx.breakerOpen.Store(true)
	if _, err := fx.run(t); err != nil {
		t.Fatal(err)
	}
	// GitHub moves again while the breaker is still open.
	_, gSHA := fakeArchive(t, "echo github 2.340.0")
	fx.ghVersion, fx.ghSHA = "2.340.0", gSHA
	fx.now = fx.now.Add(5 * time.Minute)
	_, err := fx.run(t)
	if !errors.Is(err, ErrBreakGlassCooldown) {
		t.Fatalf("within cooldown: want ErrBreakGlassCooldown, got %v", err)
	}
	if fx.gHits.Load() != 1 {
		t.Fatalf("cooldown refusal downloaded something: hits=%d", fx.gHits.Load())
	}
	// Past the cooldown the attempt proceeds (and fails on the fetch here
	// because the test server has no 2.340.0 — the point is that it TRIED).
	fx.now = fx.now.Add(15 * time.Minute)
	if _, err := fx.run(t); err == nil || errors.Is(err, ErrBreakGlassCooldown) || errors.Is(err, ErrSourcesDisagree) {
		t.Fatalf("after cooldown the fetch must be attempted, got %v", err)
	}
}

// (e) a re-signed manifest catches up: same version + hash -> no
// download, the sidecar gains the manifest source, the label reverts to
// verified-cache and the alert clears; a NEWER manifest -> a normal
// two-source upgrade replaces the break-glass tree.
func TestBreakGlass_ManifestCatchUpRevertsToSigned(t *testing.T) {
	fx := newBreakGlassFixture(t)
	fx.breakerOpen.Store(true)
	if _, err := fx.run(t); err != nil {
		t.Fatal(err)
	}
	fx.breakerOpen.Store(false) // healed; steady state again

	// Re-signed manifest: 2.339.0 with GitHub's hash, published later.
	m2 := validManifest("2.339.0", "2.339.0", fx.gSHA)
	for k, a := range m2.Platforms {
		a.URL = fx.manifest.Platforms[k].URL // any https URL; never fetched
		m2.Platforms[k] = a
	}
	m2.PublishedAt = fx.manifest.PublishedAt.Add(time.Hour)
	fx.u.Manifest = StaticManifestLoader{Manifest: m2}

	res, err := fx.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.Swapped || res.Version != "2.339.0" || !strings.Contains(res.Reason, "caught up") {
		t.Fatalf("result %+v", res)
	}
	if fx.gHits.Load() != 1 || fx.mHits.Load() != 0 {
		t.Fatal("catch-up must not download")
	}
	sc, _ := fx.store.Sidecar(fx.gSHA)
	if !sc.HasSource(SourceManifest) || !sc.HasSource(SourceGitHubDownloads) {
		t.Fatalf("sidecar after catch-up: %v", sc.Sources)
	}
	if last, _ := fx.store.LastApplied(); !last.Equal(m2.PublishedAt) {
		t.Fatalf("catch-up must record the manifest as applied, got %v", last)
	}
	if breakGlassGauge(t) != 0 || versionInfo(t, "2.339.0", metrics.RunnerSourceVerifiedCache) != 1 {
		t.Fatal("alert must clear and the label revert to verified-cache")
	}
	sel := (&Selector{Embedded: runner.New(t.TempDir(), testLog()), Store: fx.store, Log: testLog()}).Select()
	if sel.BreakGlass || sel.Source != metrics.RunnerSourceVerifiedCache {
		t.Fatalf("selector after catch-up: %+v", sel)
	}

	// A newer signed manifest later: normal two-source upgrade.
	nData, nSHA := fakeArchive(t, "echo signed 2.340.0")
	srv2, client2 := httpsTestServer(t, serveBytes(t, nData, nil))
	m3 := validManifest("2.340.0", "2.339.0", nSHA)
	for k, a := range m3.Platforms {
		a.URL = srv2.URL + "/" + ArchiveFilename(k, "2.340.0")
		m3.Platforms[k] = a
	}
	m3.PublishedAt = m2.PublishedAt.Add(time.Hour)
	fx.u.Manifest = StaticManifestLoader{Manifest: m3}
	fx.u.Fetcher.Client = client2
	fx.ghVersion, fx.ghSHA = "2.340.0", nSHA
	res, err = fx.run(t)
	if err != nil || !res.Swapped || res.BreakGlass || res.Version != "2.340.0" {
		t.Fatalf("newer manifest: %v %+v", err, res)
	}
	sc, _ = fx.store.Sidecar(nSHA)
	if len(sc.Sources) != 2 {
		t.Fatalf("two-source entry: %v", sc.Sources)
	}
	if breakGlassGauge(t) != 0 {
		t.Fatal("gauge must stay 0 after a signed upgrade")
	}
}

// A break-glass entry survives a restart with the right label: the sidecar
// is the record, not process memory.
func TestBreakGlass_LabelPersistsAcrossRestart(t *testing.T) {
	fx := newBreakGlassFixture(t)
	fx.breakerOpen.Store(true)
	if _, err := fx.run(t); err != nil {
		t.Fatal(err)
	}
	// "Restart": a fresh Store over the same directory.
	reopened := NewStore(filepath.Dir(filepath.Dir(fx.store.Root())), testLog())
	reopened.goos = "linux"
	if err := reopened.Init(); err != nil {
		t.Fatal(err)
	}
	sel := (&Selector{Embedded: runner.New(t.TempDir(), testLog()), Store: reopened, Log: testLog()}).Select()
	if !sel.BreakGlass || sel.Source != metrics.RunnerSourceGitHubBreakGlass {
		t.Fatalf("after restart: %+v", sel)
	}
}
