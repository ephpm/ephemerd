package dind

import "strings"

// buildScopeRegistry is the synthetic registry hostname used to scope
// job-built image tags inside the shared "buildkit" containerd namespace.
// It never resolves on any network — it exists only to make the scoped
// name parse as a valid Docker reference.
const buildScopeRegistry = "build.ephemerd.local"

// scopedBuildRef translates a user-supplied image tag into the job-scoped
// name that build results are stored under in the shared buildkit
// containerd namespace.
//
// All jobs share one BuildKit worker writing into one containerd
// namespace ("buildkit"). Without scoping, two concurrent jobs that both
// `docker build -t ephpm:dev` race on the same image record — last build
// wins, and the loser pushes/loads the other job's binary. Observed in
// the wild as an E2E matrix job asserting on PHP 8.4 and getting the
// 8.5 build (ephpm/ephpm#67, #68).
//
// The transform is invisible to the workflow: the job's docker CLI keeps
// using its own tag; only the storage name inside the buildkit namespace
// carries the scope. Example:
//
//	ephpm:dev → build.ephemerd.local/ephemerd-github-ephpm-quick-mendel/ephpm:dev
//
// jobID comes from the runner container ID, which containerd has already
// validated against its identifier grammar (alphanumerics, '.', '-', '_')
// — apart from uppercase, which Docker reference paths reject, so it is
// lowercased here. The user ref is left untouched: Docker already
// enforces lowercase repository names at build time, and tags are
// case-sensitive so they must not be folded. A leading registry
// qualifier on the user tag (e.g. ghcr.io/owner/img:v1) folds into the
// scoped path unchanged; both "ghcr.io" and underscores are valid
// reference path components.
func scopedBuildRef(jobID, ref string) string {
	if jobID == "" || ref == "" {
		return ref
	}
	return buildScopeRegistry + "/" + strings.ToLower(jobID) + "/" + ref
}

// pushLookupCandidates returns the ordered list of image names that a
// docker push of fullRef should try in the buildkit namespace.
//
// The Linux Docker CLI canonicalizes refs with the docker.io/ registry
// prefix before POSTing the push, but BuildKit's containerd exporter
// stores the image under whatever short name the build's `-t` tag
// carried (e.g. "ephpm/ephemerd:..." with no prefix). Job-scoped forms
// are tried first — those are where this job's own builds live. The
// unscoped fallbacks cover images staged into the buildkit namespace
// by paths other than this job's docker build (tests, future tooling).
func pushLookupCandidates(jobID, fullRef string) []string {
	stripped := strings.TrimPrefix(fullRef, "docker.io/")
	return dedup(
		scopedBuildRef(jobID, fullRef),
		scopedBuildRef(jobID, stripped),
		fullRef,
		stripped,
	)
}
