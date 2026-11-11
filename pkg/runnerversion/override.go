package runnerversion

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/runner"
)

// Image override: the Linux self-heal mechanism.
//
// For an official runner image the runtime runs the IMAGE's /home/runner
// with no runner mount (pkg/runtime: isOfficialRunnerImage,
// needsRunnerMount). When that baked-in runner is below GitHub's floor the
// fix must be a MOUNT over /home/runner, not an edit of the image layer:
// patching the overlayfs lower layer in place was tried on metal
// (2026-11-10) and the page cache kept serving the stale runner to new
// container mounts. ephemerd already has the mount machinery for custom
// images (prepareJobRunnerTree: overlayfs with a shared read-only lowerdir
// and a per-job upperdir); the override simply points it at /home/runner
// with a verified fresh runner tree as the lowerdir.
//
// ImageOverride is the host-side switch the runtime consults on every
// Create. It is nil/inactive unless the self-heal decided to engage it.

// ImageOverride holds the currently engaged override, if any. Safe for
// concurrent use; the runtime reads it on every Create.
type ImageOverride struct {
	mu     sync.RWMutex
	active bool
	dir    string
	ver    string
	image  string
	reason string
	since  time.Time
	log    *slog.Logger
}

// NewImageOverride returns an inactive override.
func NewImageOverride(log *slog.Logger) *ImageOverride {
	if log == nil {
		log = slog.Default()
	}
	return &ImageOverride{log: log}
}

// OfficialRunnerOverride is the runtime-facing lookup: for an official
// image it returns the host directory to mount over /home/runner and true,
// or "", false when no override is engaged. The image argument is recorded
// for the alert; the override applies to every official image on the node,
// since all of them are FROM the same upstream runner.
func (o *ImageOverride) OfficialRunnerOverride(image string) (string, bool) {
	if o == nil {
		return "", false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	if !o.active {
		return "", false
	}
	return o.dir, true
}

// Engage activates the override with a verified runner tree. Fires the
// fixed, grep-able alert line; the override stays engaged across jobs until
// Clear or until a rebuilt image makes it unnecessary.
func (o *ImageOverride) Engage(dir, version, image, reason string) {
	o.mu.Lock()
	o.active, o.dir, o.ver, o.image, o.reason, o.since = true, dir, version, image, reason, time.Now()
	o.mu.Unlock()
	metrics.RunnerImageOverrideActive.Set(1)
	metrics.SetRunnerVersion(version, metrics.RunnerSourceImageOverride)
	// Non-silenceable until ephemerd_runner_image_override_active returns
	// to 0, i.e. until a rebuilt runner-ci-linux image rolls out.
	o.log.Error("runner image override engaged: IMAGE RUNNER IS STALE",
		"image", image, "mounted_runner", version, "dir", dir, "reason", reason,
		"action", "rebuild and push the runner-ci-linux image with a current actions/runner, then bump the pin")
}

// Clear deactivates the override.
func (o *ImageOverride) Clear(reason string) {
	o.mu.Lock()
	was := o.active
	o.active, o.dir, o.ver = false, "", ""
	o.mu.Unlock()
	metrics.RunnerImageOverrideActive.Set(0)
	if was {
		o.log.Warn("runner image override cleared", "reason", reason)
	}
}

// Status reports the override for /healthz and logs.
func (o *ImageOverride) Status() (active bool, dir, version, image, reason string, since time.Time) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.active, o.dir, o.ver, o.image, o.reason, o.since
}

// ImageDecision is DecideImageOverride's output.
type ImageDecision struct {
	// Override is true when the image's runner should be masked by the
	// available tree.
	Override bool
	// ImageVersion is what the image ships; AvailableVersion is what would
	// be mounted over it.
	ImageVersion, AvailableVersion string
	// ImageBelowFloor is true when a manifest was given and the image is
	// older than its floor.
	ImageBelowFloor bool
	// NeedFetch is true when nothing on the node is good enough and the
	// break-glass fetch is the only way to an override.
	NeedFetch bool
	Reason    string
}

// DecideImageOverride decides whether the runner baked into an official
// image must be masked. Inputs:
//
//	image     — the image's /home/runner probe (version "" = probe failed)
//	available — the best verified tree on the node (Selector.Select)
//	m         — the verified manifest, or nil when unavailable
//	breakerOpen — the linux zero-work breaker has tripped
//
// The image is stale when it is below the manifest floor, or — with no
// manifest — when the breaker tripped and the available tree is newer. The
// override engages only when the available tree is strictly newer than the
// image AND itself at or above the floor; otherwise NeedFetch says the
// break-glass fetch is required first. A healthy image is never masked.
func DecideImageOverride(image TreeInfo, available Selection, m *Manifest, breakerOpen bool) ImageDecision {
	d := ImageDecision{ImageVersion: image.Version, AvailableVersion: available.Version}
	if image.Version == "" || len(image.Parts) == 0 {
		d.Reason = "image runner version unknown"
		return d
	}
	if m != nil {
		d.ImageBelowFloor = m.BelowFloor(image.Parts)
	}
	stale := d.ImageBelowFloor || (m == nil && breakerOpen)
	if !stale {
		d.Reason = "image runner is not stale"
		return d
	}
	availNewer := len(available.Parts) > 0 && runner.CompareVersions(available.Parts, image.Parts) > 0
	availOK := availNewer && (m == nil || !m.BelowFloor(available.Parts))
	if !availOK {
		d.NeedFetch = true
		d.Reason = fmt.Sprintf("image runner %s is stale and no verified tree on the node is newer and at/above the floor", image.Version)
		return d
	}
	d.Override = true
	switch {
	case d.ImageBelowFloor:
		d.Reason = fmt.Sprintf("image runner %s is below manifest floor %s; mounting %s", image.Version, m.Floor, available.Version)
	default:
		d.Reason = fmt.Sprintf("linux breaker open and image runner %s is older than %s; mounting it", image.Version, available.Version)
	}
	return d
}

// PrepareOverrideTree makes a verified runner tree usable as the lowerdir of
// an overlay over the image's /home/runner: the image's unprivileged
// `runner` user (uid/gid from the probe) must own it, or the merged
// directory inherits root ownership from the lower and the runner cannot
// create _work/. No-op when the owner is unknown.
func PrepareOverrideTree(dir string, image TreeInfo) error {
	if image.UID < 0 || image.GID < 0 {
		return nil
	}
	if err := chownTree(dir, image.UID, image.GID); err != nil {
		return fmt.Errorf("chowning override tree %s to %d:%d: %w", dir, image.UID, image.GID, err)
	}
	return nil
}
