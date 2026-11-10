package runtime

import "path/filepath"

// Self-healing runner version: the official-image override seam.
//
// For an official runner image (isOfficialRunnerImage) the container runs
// the runner baked into the image at /home/runner, with NO runner mount —
// so the embedded/extracted runner never touches that path. When that
// baked-in runner falls below GitHub's minimum version every Linux job
// dies (2026-11-10: runner-ci-linux shipped 2.336.0). Editing the image's
// overlayfs lower layer in place was tried and is unreliable: the page
// cache keeps serving the stale files to new container mounts. The fix is
// the mount machinery that already exists for custom images, pointed at
// /home/runner: an overlayfs whose read-only lowerdir is a verified fresh
// runner tree and whose upperdir is per-job (prepareJobRunnerTree).
//
// RunnerOverrideSource is what Create consults. Config.RunnerOverride is
// nil unless the self-heal subsystem is enabled and engaged
// (pkg/runnerversion.ImageOverride); with nil the plan is exactly what it
// was before this seam existed. See docs/arch/self-healing-runner.md.

// RunnerOverrideSource answers, per Create, whether an official image's
// baked-in runner must be masked by a host runner tree.
type RunnerOverrideSource interface {
	// OfficialRunnerOverride returns the host directory to mount over the
	// official runner root (/home/runner) and true, or "", false when the
	// image's own runner should run.
	OfficialRunnerOverride(image string) (dir string, ok bool)
}

// runnerMountPlan is the per-job runner mount decision.
type runnerMountPlan struct {
	// needsMount is the former needsRunnerMount: install a runner tree in
	// the container at mount, from the host tree at dir.
	needsMount bool
	dir        string // host runner tree (overlay lowerdir / Windows copy source)
	mount      string // container path
	// jobRoot is the host directory the per-job writable tree
	// ("job-<id>": the overlay upper/work on Linux, the byte copy on
	// Windows) is created under. It is ALWAYS the configured runners
	// directory (the parent of RunnerDir) when one is set: Destroy and the
	// orphan sweep look for job-* there, and the verified store prunes
	// everything it does not own under its own extracted/ root — a job
	// tree created next to an override lowerdir would be invisible to the
	// former and deleted by the latter.
	jobRoot string
	// override is true when the mount masks an official image's runner.
	override bool
}

// planRunnerMount is the pure decision. The historical rule is kept
// verbatim — custom images and Windows always mount the configured runner —
// and one case is added: an official image on Linux with an engaged
// override mounts the override tree at the official runner root instead of
// nothing. Windows has no official-image path (there is no Windows runner
// image), so the override never applies there.
func planRunnerMount(customImage bool, goos, cfgDir, cfgMount string, src RunnerOverrideSource, image string) runnerMountPlan {
	if (customImage || goos == "windows") && cfgDir != "" && cfgMount != "" {
		return runnerMountPlan{needsMount: true, dir: cfgDir, mount: cfgMount, jobRoot: filepath.Dir(cfgDir)}
	}
	if !customImage && goos != "windows" && src != nil {
		if dir, ok := src.OfficialRunnerOverride(image); ok && dir != "" {
			jobRoot := filepath.Dir(dir)
			if cfgDir != "" {
				jobRoot = filepath.Dir(cfgDir)
			}
			return runnerMountPlan{needsMount: true, dir: dir, mount: officialRunnerRoot, jobRoot: jobRoot, override: true}
		}
	}
	return runnerMountPlan{}
}
