package runnerversion

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

type upgradeFixture struct {
	u        *Upgrader
	store    *Store
	hits     *atomic.Int32
	manifest *Manifest
}

// newUpgradeFixture serves `data` for the manifest's linux-x64 URL and
// returns an upgrader whose manifest and GitHub source both describe it.
func newUpgradeFixture(t *testing.T, version string, data []byte, sha string) *upgradeFixture {
	t.Helper()
	var hits atomic.Int32
	srv, client := httpsTestServer(t, serveBytes(t, data, &hits))
	m := validManifest(version, version, sha)
	for k, a := range m.Platforms {
		a.URL = srv.URL + "/" + ArchiveFilename(k, version)
		m.Platforms[k] = a
	}
	// validManifest URLs are example.com; the test server's host is an
	// IP:port which Validate accepts (https + basename).
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t)
	fx := &upgradeFixture{store: store, hits: &hits, manifest: m}
	fx.u = &Upgrader{
		AutoUpgrade: true,
		Channel:     ChannelStable,
		Platform:    "linux-x64",
		Manifest:    StaticManifestLoader{Manifest: m},
		GitHub: &GitHubSource{list: func(context.Context) ([]*gh.RunnerApplicationDownload, error) {
			return ghDownloads(version, sha), nil
		}},
		Fetcher: &Fetcher{Store: store, Client: client, DiskCheck: plentyOfDisk, Log: testLog()},
		Store:   store,
		Log:     testLog(),
	}
	return fx
}

func TestUpgraderDisabledTouchesNothing(t *testing.T) {
	data, sha := fakeArchive(t, "echo x")
	fx := newUpgradeFixture(t, "2.338.2", data, sha)
	fx.u.AutoUpgrade = false
	_, err := fx.u.Run(context.Background())
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
	if fx.hits.Load() != 0 {
		t.Fatal("disabled upgrader hit the network")
	}
	if _, ok, _ := fx.store.Current(); ok {
		t.Fatal("disabled upgrader changed the store")
	}
}

func TestUpgraderHappyPath(t *testing.T) {
	data, sha := fakeArchive(t, "echo new")
	fx := newUpgradeFixture(t, "2.338.2", data, sha)
	res, err := fx.u.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Swapped || res.Version != "2.338.2" || res.SHA256 != sha || !strings.Contains(res.Dir, "2.338.2-linux") {
		t.Fatalf("result %+v", res)
	}
	cur, ok, _ := fx.store.Current()
	if !ok || cur.SHA256 != sha {
		t.Fatalf("current %+v %v", cur, ok)
	}
	last, _ := fx.store.LastApplied()
	if !last.Equal(fx.manifest.PublishedAt) {
		t.Fatalf("last applied %v", last)
	}
	sc, _ := fx.store.Sidecar(sha)
	if len(sc.Sources) != 2 || sc.Sources[0] != SourceManifest || sc.Sources[1] != SourceGitHubDownloads {
		t.Fatalf("sidecar sources %v", sc.Sources)
	}

	// Same manifest again: replay guard (published_at not newer).
	_, err = fx.u.Run(context.Background())
	if !errors.Is(err, ErrManifestReplayed) {
		t.Fatalf("expected ErrManifestReplayed, got %v", err)
	}
	// A newer manifest for the same artifact: nothing to do, no fetch.
	fx.manifest.PublishedAt = fx.manifest.PublishedAt.Add(time.Hour)
	res, err = fx.u.Run(context.Background())
	if err != nil || res.Swapped || res.Reason != "already current" {
		t.Fatalf("already current: %v %+v", err, res)
	}
	if fx.hits.Load() != 1 {
		t.Fatalf("server hits %d, want 1", fx.hits.Load())
	}
}

func TestUpgraderRefusesWhenSourcesDisagree(t *testing.T) {
	data, sha := fakeArchive(t, "echo x")
	fx := newUpgradeFixture(t, "2.338.2", data, sha)

	// GitHub says a newer version than the manifest: manifest is stale.
	fx.u.GitHub = &GitHubSource{list: func(context.Context) ([]*gh.RunnerApplicationDownload, error) {
		return ghDownloads("2.338.3", sha), nil
	}}
	_, err := fx.u.Run(context.Background())
	if !errors.Is(err, ErrSourcesDisagree) {
		t.Fatalf("version disagreement: %v", err)
	}
	// Same version, different hash: someone is lying.
	fx.u.GitHub = &GitHubSource{list: func(context.Context) ([]*gh.RunnerApplicationDownload, error) {
		return ghDownloads("2.338.2", zeroSHA), nil
	}}
	_, err = fx.u.Run(context.Background())
	if !errors.Is(err, ErrSourcesDisagree) {
		t.Fatalf("hash disagreement: %v", err)
	}
	if fx.hits.Load() != 0 {
		t.Fatal("must not fetch when sources disagree")
	}
}

func TestUpgraderRefusesBadBytesEvenWhenSourcesAgree(t *testing.T) {
	// Both sources agree on a hash the served bytes do not match.
	data, _ := fakeArchive(t, "echo served")
	fx := newUpgradeFixture(t, "2.338.2", data, zeroSHA)
	_, err := fx.u.Run(context.Background())
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
	if _, ok, _ := fx.store.Current(); ok {
		t.Fatal("bad bytes must not become current")
	}
	if blobs := listBlobs(t, fx.store); len(blobs) != 0 {
		t.Fatalf("blobs after bad fetch: %v", blobs)
	}
}

func TestUpgraderChannelAndDowngrade(t *testing.T) {
	data, sha := fakeArchive(t, "echo x")
	fx := newUpgradeFixture(t, "2.338.2", data, sha)
	if _, err := fx.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Canary manifest on a stable node.
	fx.manifest.Channel = ChannelCanary
	fx.manifest.PublishedAt = fx.manifest.PublishedAt.Add(time.Hour)
	if _, err := fx.u.Run(context.Background()); !errors.Is(err, ErrWrongChannel) {
		t.Fatalf("channel: %v", err)
	}
	fx.manifest.Channel = ChannelStable

	// A newer manifest that points at an OLDER version without rollback.
	oldData, oldSHA := fakeArchive(t, "echo old")
	old := newUpgradeFixture(t, "2.338.0", oldData, oldSHA)
	old.manifest.PublishedAt = fx.manifest.PublishedAt.Add(2 * time.Hour)
	fx.u.Manifest = StaticManifestLoader{Manifest: old.manifest}
	fx.u.GitHub = old.u.GitHub
	fx.u.Fetcher = old.u.Fetcher
	fx.u.Fetcher.Store = fx.store
	if _, err := fx.u.Run(context.Background()); !errors.Is(err, ErrDowngradeRefused) {
		t.Fatalf("downgrade: %v", err)
	}
	// With rollback=true it is allowed.
	old.manifest.Rollback = true
	res, err := fx.u.Run(context.Background())
	if err != nil || !res.Swapped || res.Version != "2.338.0" {
		t.Fatalf("rollback: %v %+v", err, res)
	}
}

func TestUpgraderManifestLoadFailure(t *testing.T) {
	data, sha := fakeArchive(t, "echo x")
	fx := newUpgradeFixture(t, "2.338.2", data, sha)
	fx.u.Manifest = StaticManifestLoader{}
	if _, err := fx.u.Run(context.Background()); !errors.Is(err, ErrManifestFetch) {
		t.Fatalf("expected ErrManifestFetch, got %v", err)
	}
	if fx.hits.Load() != 0 {
		t.Fatal("no manifest, no fetch")
	}
}
