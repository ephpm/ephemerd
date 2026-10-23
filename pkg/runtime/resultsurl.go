package runtime

// officialRunnerRoot is where ghcr.io/actions/actions-runner (and the
// runner-ci-* images built on it) keep the runner. Jobs on those images run
// the image's own runner, not ephemerd's embedded one, so the LAN-cache patch
// has to be applied inside the container's snapshot — see
// patchSnapshotRunner.
const officialRunnerRoot = "/home/runner"

// logResultsPatch reports how the LAN-cache runner patch went for one job.
// It never fails the job: an unpatched runner overwrites the cache URL with
// GitHub's, so the worst case is the job using GitHub's cache as before.
func (r *Runtime) logResultsPatch(id, which string) func(bool, error) {
	return func(patched bool, err error) {
		switch {
		case err != nil:
			r.cfg.Log.Warn("could not patch the runner for the LAN Actions cache; this job uses GitHub's cache", "id", id, "runner", which, "error", err)
		case !patched:
			r.cfg.Log.Warn("runner does not export ACTIONS_RESULTS_URL where expected (new runner version?); this job uses GitHub's cache", "id", id, "runner", which)
		default:
			r.cfg.Log.Info("runner patched for the LAN Actions cache", "id", id, "runner", which)
		}
	}
}
