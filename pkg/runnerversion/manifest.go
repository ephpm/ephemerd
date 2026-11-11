// Package runnerversion is the self-healing runner version subsystem: a
// signed manifest describing the runner the fleet should be on, a verified
// content-addressed cache of runner archives, a selector that chooses
// between the embedded runner and a verified cached one, and a detector
// for runners that register and exit without ever receiving a job.
//
// See docs/arch/self-healing-runner.md. Everything here is inert until
// [runner.self_heal] enabled = true, and nothing in this package is wired
// into the daemon yet.
package runnerversion

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ephpm/ephemerd/pkg/runner"
)

// SchemaVersion is the only manifest schema this build understands. An
// unknown schema is rejected outright rather than best-effort parsed: a
// manifest is a security input, and "mostly understood" is not understood.
const SchemaVersion = 1

// Channel selects which manifests a node follows.
type Channel string

const (
	ChannelStable Channel = "stable"
	ChannelCanary Channel = "canary"
)

// Platform is the runner's own platform naming (the same scheme
// mage/download uses for archive filenames): "linux-x64", "win-x64",
// "osx-arm64", ...
var platformRe = regexp.MustCompile(`^(linux|win|osx)-(x64|arm64)$`)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PlatformKey maps GOOS/GOARCH to the manifest platform key.
func PlatformKey(goos, goarch string) string {
	var os_ string
	switch goos {
	case "windows":
		os_ = "win"
	case "darwin":
		os_ = "osx"
	default:
		os_ = "linux"
	}
	arch := "x64"
	if goarch == "arm64" {
		arch = "arm64"
	}
	return os_ + "-" + arch
}

// ArchiveExt is the archive extension the runner ships for a platform key.
func ArchiveExt(platform string) string {
	if strings.HasPrefix(platform, "win-") {
		return "zip"
	}
	return "tar.gz"
}

// ArchiveFilename is the upstream release asset name for a platform+version.
func ArchiveFilename(platform, version string) string {
	return fmt.Sprintf("actions-runner-%s-%s.%s", platform, version, ArchiveExt(platform))
}

// PlatformArtifact is one platform's archive in a Manifest.
type PlatformArtifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Manifest is the signed runner-manifest.json document.
type Manifest struct {
	SchemaVersion int                         `json:"schema_version"`
	Version       string                      `json:"version"`
	Floor         string                      `json:"floor"`
	Channel       Channel                     `json:"channel"`
	Rollback      bool                        `json:"rollback"`
	PublishedAt   time.Time                   `json:"published_at"`
	Platforms     map[string]PlatformArtifact `json:"platforms"`
}

// ErrManifestInvalid wraps every validation failure.
var ErrManifestInvalid = errors.New("runner manifest invalid")

// ParseManifest decodes and validates a manifest. It does NOT verify a
// signature — callers must verify the bytes first (see PublicKey.Verify
// and VerifyAndParse); this is the parsing half only.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrManifestInvalid, err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest's internal consistency.
func (m *Manifest) Validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrManifestInvalid, fmt.Sprintf(format, args...))
	}
	if m.SchemaVersion != SchemaVersion {
		return fail("schema_version %d not supported (want %d)", m.SchemaVersion, SchemaVersion)
	}
	ver := runner.ParseVersionString(m.Version)
	if len(ver) != 3 {
		return fail("version %q is not X.Y.Z", m.Version)
	}
	floor := runner.ParseVersionString(m.Floor)
	if len(floor) != 3 {
		return fail("floor %q is not X.Y.Z", m.Floor)
	}
	if runner.CompareVersions(floor, ver) > 0 {
		return fail("floor %s is above version %s", m.Floor, m.Version)
	}
	switch m.Channel {
	case ChannelStable, ChannelCanary:
	default:
		return fail("channel %q is not stable|canary", m.Channel)
	}
	if m.PublishedAt.IsZero() {
		return fail("published_at missing")
	}
	if len(m.Platforms) == 0 {
		return fail("platforms is empty")
	}
	keys := make([]string, 0, len(m.Platforms))
	for k := range m.Platforms {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := m.Platforms[k]
		if !platformRe.MatchString(k) {
			return fail("platform key %q is not <linux|win|osx>-<x64|arm64>", k)
		}
		if !sha256Re.MatchString(a.SHA256) {
			return fail("platform %s: sha256 is not 64 lower-case hex chars", k)
		}
		u, err := url.Parse(a.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fail("platform %s: url must be https", k)
		}
		if want := ArchiveFilename(k, m.Version); path.Base(u.Path) != want {
			return fail("platform %s: url basename %q is not %q", k, path.Base(u.Path), want)
		}
	}
	return nil
}

// Artifact returns the entry for a platform key.
func (m *Manifest) Artifact(platform string) (PlatformArtifact, error) {
	a, ok := m.Platforms[platform]
	if !ok {
		return PlatformArtifact{}, fmt.Errorf("%w: no entry for platform %s", ErrManifestInvalid, platform)
	}
	return a, nil
}

// VersionParts is Version parsed for comparison.
func (m *Manifest) VersionParts() []int { return runner.ParseVersionString(m.Version) }

// FloorParts is Floor parsed for comparison.
func (m *Manifest) FloorParts() []int { return runner.ParseVersionString(m.Floor) }

// BelowFloor reports whether v is older than the manifest floor.
func (m *Manifest) BelowFloor(v []int) bool {
	return runner.CompareVersions(v, m.FloorParts()) < 0
}
