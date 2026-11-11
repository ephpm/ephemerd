package runnerversion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/runner"
)

func withEmbeddedVersion(t *testing.T, v string) {
	t.Helper()
	old := runner.Version
	runner.Version = v
	t.Cleanup(func() { runner.Version = old })
}

func swapIn(t *testing.T, s *Store, version string) string {
	t.Helper()
	sha, _ := putArchive(t, s, version, "echo "+version)
	dir, err := s.Extract(sha, version)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Swap(sha, version); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSelectorEmbeddedIsTheFloor(t *testing.T) {
	withEmbeddedVersion(t, "2.337.0")
	rm := runner.New(t.TempDir(), testLog())
	sel := &Selector{Embedded: rm, Log: testLog()}

	// No store at all: embedded.
	if got := sel.Select(); got.Source != metrics.RunnerSourceEmbedded || got.Dir != rm.Dir() || got.Version != "2.337.0" {
		t.Fatalf("no store: %+v", got)
	}
	// Empty store: embedded.
	sel.Store = newTestStore(t)
	if got := sel.Select(); got.Source != metrics.RunnerSourceEmbedded {
		t.Fatalf("empty store: %+v", got)
	}
	// Cache LOWER than embedded: embedded.
	swapIn(t, sel.Store, "2.336.0")
	if got := sel.Select(); got.Source != metrics.RunnerSourceEmbedded {
		t.Fatalf("lower cache: %+v", got)
	}
	// Cache EQUAL: embedded (the pinned release retires the cache entry).
	swapIn(t, sel.Store, "2.337.0")
	if got := sel.Select(); got.Source != metrics.RunnerSourceEmbedded {
		t.Fatalf("equal cache: %+v", got)
	}
	// Cache HIGHER: cache.
	dir := swapIn(t, sel.Store, "2.338.2")
	got := sel.Select()
	if got.Source != metrics.RunnerSourceVerifiedCache || got.Dir != dir || got.Version != "2.338.2" {
		t.Fatalf("higher cache: %+v", got)
	}
	if sel.Dir() != dir {
		t.Fatal("Dir() should follow Select()")
	}
	// Cache higher but its extraction vanished: embedded.
	if err := os.Remove(filepath.Join(dir, extractedMarker)); err != nil {
		t.Fatal(err)
	}
	if got := sel.Select(); got.Source != metrics.RunnerSourceEmbedded {
		t.Fatalf("missing marker: %+v", got)
	}
}

func TestSelectorBootVerify(t *testing.T) {
	withEmbeddedVersion(t, "2.337.0")
	rm := runner.New(t.TempDir(), testLog())
	const embSHA = "1111111111111111111111111111111111111111111111111111111111111111"
	sel := &Selector{Embedded: rm, Store: newTestStore(t), Log: testLog(), HashEmbedded: func() (string, error) { return embSHA, nil }}

	// No manifest: hashes only, nothing below floor.
	rep, err := sel.BootVerify(nil, "linux-x64")
	if err != nil || rep.EmbeddedSHA256 != embSHA || rep.BelowFloor || rep.EmbeddedMatchesManifest {
		t.Fatalf("nil manifest: %v %+v", err, rep)
	}

	// Manifest lists the embedded version with the right hash, floor above it.
	m := validManifest("2.337.0", "2.337.0", embSHA)
	m.Floor = "2.337.0"
	rep, err = sel.BootVerify(m, "linux-x64")
	if err != nil || !rep.EmbeddedMatchesManifest || rep.EmbeddedTampered || rep.BelowFloor {
		t.Fatalf("matching: %v %+v", err, rep)
	}

	// Manifest lists the embedded version with a DIFFERENT hash: tampered.
	m = validManifest("2.337.0", "2.337.0", zeroSHA)
	_, err = sel.BootVerify(m, "linux-x64")
	if err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("tampered should error: %v", err)
	}

	// Manifest for a newer version with a floor above embedded: below floor.
	m = validManifest("2.338.2", "2.338.0", zeroSHA)
	rep, err = sel.BootVerify(m, "linux-x64")
	if err != nil || !rep.BelowFloor || rep.EmbeddedTampered || rep.Selected.Source != metrics.RunnerSourceEmbedded {
		t.Fatalf("below floor: %v %+v", err, rep)
	}

	// A verified cache entry at the manifest version lifts us above the floor.
	swapIn(t, sel.Store, "2.338.2")
	rep, err = sel.BootVerify(m, "linux-x64")
	if err != nil || rep.BelowFloor || rep.Selected.Source != metrics.RunnerSourceVerifiedCache || len(rep.Cache) != 1 || !rep.Cache[0].OK {
		t.Fatalf("cache lifts floor: %v %+v", err, rep)
	}

	// Hashing the embed fails: error out, do not guess.
	sel.HashEmbedded = func() (string, error) { return "", os.ErrNotExist }
	if _, err := sel.BootVerify(m, "linux-x64"); err == nil {
		t.Fatal("expected hash error")
	}
}

func TestSelectorBootVerifyImagePath(t *testing.T) {
	withEmbeddedVersion(t, "2.337.0")
	rm := runner.New(t.TempDir(), testLog())
	m := validManifest("2.338.2", "2.338.0", zeroSHA)
	sel := &Selector{
		Embedded:     rm,
		Store:        newTestStore(t),
		Log:          testLog(),
		HashEmbedded: func() (string, error) { return zeroSHA, nil },
		ProbeImage:   func() (TreeInfo, error) { return tree("2.336.0"), nil },
	}

	// The outage: image 2.336.0 and embedded 2.337.0 both below floor 2.338.0.
	rep, err := sel.BootVerify(m, "linux-x64")
	if err != nil || rep.Image == nil || rep.ImageErr != nil {
		t.Fatalf("%v %+v", err, rep)
	}
	if rep.Image.Override || !rep.Image.NeedFetch || !rep.Image.ImageBelowFloor {
		t.Fatalf("both below floor: %+v", rep.Image)
	}

	// After the break-glass fetch: the cache tree is what gets mounted.
	swapIn(t, sel.Store, "2.338.2")
	rep, err = sel.BootVerify(m, "linux-x64")
	if err != nil || rep.Image == nil {
		t.Fatalf("%v %+v", err, rep)
	}
	if !rep.Image.Override || rep.Image.AvailableVersion != "2.338.2" || rep.Image.NeedFetch {
		t.Fatalf("cache lifts image: %+v", rep.Image)
	}

	// A probe failure is reported and never turns into an override.
	sel.ProbeImage = func() (TreeInfo, error) { return TreeInfo{}, os.ErrNotExist }
	rep, err = sel.BootVerify(m, "linux-x64")
	if err != nil || rep.Image != nil || rep.ImageErr == nil {
		t.Fatalf("probe failure: %v %+v", err, rep)
	}
}
