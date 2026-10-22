package dind

import "strings"

// Names the runner exports the results service address under: the real one,
// and the one a runner patched for the LAN Actions cache exports instead
// (pkg/runner.PatchResultsURL). Duplicated as literals so dind does not
// depend on the runner package.
const (
	resultsURLVar     = "ACTIONS_RESULTS_URL="
	resultsRenamedVar = "ACTIONS_RESULTS_ORL="
)

// translateResultsEnv fixes ACTIONS_RESULTS_URL for a `container:` job.
//
// Host steps inherit the runner process's environment, which carries the
// ACTIONS_RESULTS_URL ephemerd set for the LAN cache. Container steps do
// not: the runner hands them only the step environment, through
// `docker exec -e` — and a patched runner exports GitHub's address there as
// ACTIONS_RESULTS_ORL. Left alone, a container step would see no
// ACTIONS_RESULTS_URL at all, and upload-artifact would fail the job.
//
// So whenever the renamed variable is present, ACTIONS_RESULTS_URL is set to
// the LAN cache (resultsURL) when this job has one, or restored to GitHub's
// address when it does not. Env without the renamed variable — every job not
// using the LAN cache — is returned untouched.
func translateResultsEnv(env []string, resultsURL string) []string {
	original, found := "", false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, resultsRenamedVar); ok {
			original, found = v, true
		}
	}
	if !found {
		return env
	}
	want := resultsURL
	if want == "" {
		want = original
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, resultsURLVar) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, resultsURLVar+want)
}
