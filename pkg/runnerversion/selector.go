package runnerversion

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/runner"
)

// Selector chooses the runner directory to serve: the highest version among
// the embedded runner and the verified cache's current entry, with the
// embedded runner as the last-resort floor.
//
// Integration (cmd/ephemerd/selfheal.go): runtime.Config.RunnerDir stays
// the embedded tree; the runtime consults MountedRunnerDir on every Create
// (pkg/runtime.MountedRunnerSource) so a verified-cache runner takes effect
// on the next job. The Selector wraps *runner.Manager; it does not change
// runner.go's own selection among embedded archives.
type Selector struct {
	Embedded *runner.Manager
	Store    *Store
	Log      *slog.Logger
	// HashEmbedded overrides how BootVerify hashes the embedded archive
	// (tests: the test binary embeds no runner). Nil = read it from the
	// embedded FS.
	HashEmbedded func() (string, error)
	// ProbeImage reads the runner baked into the official Linux image this
	// node dispatches to (runtime.ProbeImageRunner). Nil = no image path on
	// this node (Windows/macOS hosts, or a custom default image). This is
	// the dominant path on the fleet: see override.go.
	ProbeImage func() (TreeInfo, error)
	// BreakerOpen reports whether the linux zero-work breaker has tripped;
	// consulted by the image decision when no manifest is available.
	BreakerOpen func() bool
}

func (s *Selector) hashEmbedded() (string, error) {
	if s.HashEmbedded != nil {
		return s.HashEmbedded()
	}
	name, f, err := s.Embedded.EmbeddedArchive()
	if err != nil {
		return "", err
	}
	sha, hashErr := HashReader(f)
	if err := f.Close(); err != nil && hashErr == nil {
		hashErr = err
	}
	if hashErr != nil {
		return "", fmt.Errorf("hashing embedded runner %s: %w", name, hashErr)
	}
	return sha, nil
}

// Selection is the chosen runner.
type Selection struct {
	Dir     string
	Version string
	Parts   []int
	// Source is metrics.RunnerSourceEmbedded, RunnerSourceVerifiedCache or
	// RunnerSourceGitHubBreakGlass.
	Source string
	// BreakGlass is true when the cache entry was fetched on GitHub's word
	// alone and no signed manifest has covered it yet (its sidecar lists
	// no manifest source). Read from the sidecar on every Select, so the
	// label is right across restarts and clears the moment a manifest
	// catches up.
	BreakGlass bool
}

// Select picks the runner. It never returns an error: with no usable cache
// entry the embedded runner wins, which is exactly today's behaviour.
func (s *Selector) Select() Selection {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	emb := Selection{
		Dir:     s.Embedded.Dir(),
		Version: runner.Version,
		Parts:   runner.ParseVersionString(runner.Version),
		Source:  metrics.RunnerSourceEmbedded,
	}
	if s.Store == nil {
		return emb
	}
	cur, ok, err := s.Store.Current()
	if err != nil {
		log.Warn("reading verified-cache pointer; using embedded runner", "error", err)
		return emb
	}
	if !ok {
		return emb
	}
	if _, err := os.Stat(filepath.Join(cur.Dir, extractedMarker)); err != nil {
		log.Warn("verified-cache runner not extracted; using embedded runner", "version", cur.Version, "dir", cur.Dir)
		return emb
	}
	cparts := runner.ParseVersionString(cur.Version)
	// Strictly higher wins. Equal prefers embedded: once a release ships
	// the pin, the cache entry retires on its own and Prune removes it.
	if runner.CompareVersions(cparts, emb.Parts) <= 0 {
		return emb
	}
	sel := Selection{Dir: cur.Dir, Version: cur.Version, Parts: cparts, Source: metrics.RunnerSourceVerifiedCache}
	if sc, err := s.Store.Sidecar(cur.SHA256); err == nil && sc.IsBreakGlass() {
		sel.Source, sel.BreakGlass = metrics.RunnerSourceGitHubBreakGlass, true
	}
	return sel
}

// Dir is Select().Dir and also publishes ephemerd_runner_version_info.
func (s *Selector) Dir() string {
	sel := s.Select()
	metrics.SetRunnerVersion(sel.Version, sel.Source)
	return sel.Dir
}

// BootReport is the outcome of BootVerify.
type BootReport struct {
	Selected Selection
	// EmbeddedSHA256 is the hash of the embedded archive this boot.
	EmbeddedSHA256 string
	// EmbeddedMatchesManifest is true when the manifest lists the embedded
	// version for this platform and the hashes agree. False with a nil
	// manifest or when the manifest is for a different version.
	EmbeddedMatchesManifest bool
	// EmbeddedTampered is true when the manifest lists the embedded
	// version and the hashes DISAGREE. That is a build or supply-chain
	// problem, not a floor problem.
	EmbeddedTampered bool
	// BelowFloor is true when the selected runner is older than the
	// manifest floor and must not take work.
	BelowFloor bool
	// Cache holds the store's verification results (nil without a store).
	Cache []VerifyResult
	// Image is the official-image probe and override decision (nil when
	// Selector.ProbeImage is nil). ImageErr is a failed probe. ImageInfo
	// is the raw probe (version + owner), kept so the controller can
	// prepare an override tree and re-decide after a fetch without
	// probing again.
	Image     *ImageDecision
	ImageInfo TreeInfo
	ImageErr  error
}

// MountedRunnerDir is the runtime-facing lookup for the MOUNTED runner
// paths (custom Linux images, Windows): it returns the verified-cache
// runner directory and true when the cache holds a runner strictly newer
// than the embedded one, or "", false when the configured (embedded) tree
// should be mounted. Consulted on every Create, so a break-glass fetch
// takes effect on the next job without a restart. It satisfies
// pkg/runtime.MountedRunnerSource.
func (s *Selector) MountedRunnerDir() (string, bool) {
	sel := s.Select()
	if sel.Source == metrics.RunnerSourceEmbedded {
		return "", false
	}
	return sel.Dir, true
}

// BootVerify is mechanism 1: re-hash the embedded archive, verify the
// cache's blobs, select, and compare against the manifest floor. m may be
// nil (manifest unavailable), in which case only the hash checks run.
func (s *Selector) BootVerify(m *Manifest, platform string) (BootReport, error) {
	var rep BootReport
	sha, err := s.hashEmbedded()
	if err != nil {
		return rep, err
	}
	rep.EmbeddedSHA256 = sha

	if s.Store != nil {
		rep.Cache = s.Store.Verify()
	}

	if m != nil {
		if a, err := m.Artifact(platform); err == nil && m.Version == runner.Version {
			rep.EmbeddedMatchesManifest = a.SHA256 == sha
			rep.EmbeddedTampered = !rep.EmbeddedMatchesManifest
		}
	}

	rep.Selected = s.Select()
	if m != nil {
		rep.BelowFloor = m.BelowFloor(rep.Selected.Parts)
	}
	if rep.EmbeddedTampered {
		return rep, fmt.Errorf("embedded runner %s hashes to %s but the signed manifest says %s for %s",
			runner.Version, sha, m.Platforms[platform].SHA256, runner.Version)
	}
	source := rep.Selected.Source
	if rep.BelowFloor {
		source = metrics.RunnerSourceBelowFloor
	}
	metrics.SetRunnerVersion(rep.Selected.Version, source)

	// The Linux official-image path: what the jobs ACTUALLY run. A probe
	// failure is reported, not fatal — the node keeps serving as today.
	if s.ProbeImage != nil {
		img, err := s.ProbeImage()
		if err != nil {
			rep.ImageErr = err
		} else {
			rep.ImageInfo = img
			breaker := false
			if s.BreakerOpen != nil {
				breaker = s.BreakerOpen()
			}
			d := DecideImageOverride(img, rep.Selected, m, breaker)
			rep.Image = &d
		}
	}
	return rep, nil
}

// ErrBelowFloor is a sentinel for callers that turn BootReport.BelowFloor
// into a refusal to serve.
var ErrBelowFloor = errors.New("selected runner is below the manifest floor")
