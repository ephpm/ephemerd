package dind

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/distribution/reference"
)

func TestScopedBuildRef(t *testing.T) {
	tests := []struct {
		name  string
		jobID string
		ref   string
		want  string
	}{
		{
			name:  "simple tag",
			jobID: "ephemerd-github-ephpm-quick-mendel",
			ref:   "ephpm:dev",
			want:  "build.ephemerd.local/ephemerd-github-ephpm-quick-mendel/ephpm:dev",
		},
		{
			name:  "repo path",
			jobID: "job1",
			ref:   "ephpm/e2e:dev",
			want:  "build.ephemerd.local/job1/ephpm/e2e:dev",
		},
		{
			name:  "registry-qualified ref folds into path",
			jobID: "job1",
			ref:   "ghcr.io/ephpm/ephemerd:v1",
			want:  "build.ephemerd.local/job1/ghcr.io/ephpm/ephemerd:v1",
		},
		{
			name:  "uppercase jobID is lowercased",
			jobID: "Job-ABC",
			ref:   "img:tag",
			want:  "build.ephemerd.local/job-abc/img:tag",
		},
		{
			name:  "case-sensitive tag preserved",
			jobID: "job1",
			ref:   "img:V1.2-RC",
			want:  "build.ephemerd.local/job1/img:V1.2-RC",
		},
		{
			name:  "empty jobID is passthrough",
			jobID: "",
			ref:   "img:tag",
			want:  "img:tag",
		},
		{
			name:  "empty ref is passthrough",
			jobID: "job1",
			ref:   "",
			want:  "",
		},
		{
			name:  "underscored job id",
			jobID: "ephemerd-github-ephpm-quick_mendel",
			ref:   "ephpm:dev",
			want:  "build.ephemerd.local/ephemerd-github-ephpm-quick_mendel/ephpm:dev",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scopedBuildRef(tt.jobID, tt.ref)
			if got != tt.want {
				t.Errorf("scopedBuildRef(%q, %q) = %q, want %q", tt.jobID, tt.ref, got, tt.want)
			}
		})
	}
}

// TestScopedBuildRef_ProducesValidReferences asserts the scoped names parse
// under the Docker reference grammar — BuildKit's containerd image exporter
// rejects invalid refs, so a scoping scheme that produces unparseable names
// would break every docker build.
func TestScopedBuildRef_ProducesValidReferences(t *testing.T) {
	jobIDs := []string{
		"ephemerd-github-ephpm-quick-mendel",
		"ephemerd-github-ephpm-quick_mendel",
		"ephemerd-github-php-sdk-noble_galileo",
	}
	refs := []string{
		"ephpm:dev",
		"ephpm-e2e:dev",
		"ephpm/sub:v1.2.3",
		"ghcr.io/ephpm/ephemerd:runner-ci-linux-amd64",
		"img:UPPER-Tag_ok.1",
	}
	for _, j := range jobIDs {
		for _, r := range refs {
			scoped := scopedBuildRef(j, r)
			if _, err := reference.ParseNormalizedNamed(scoped); err != nil {
				t.Errorf("scoped ref %q does not parse: %v", scoped, err)
			}
		}
	}
}

// TestDockerBuildOptsToSolveOpt_ScopesTags asserts the build translation
// rewrites every -t tag into its job-scoped form, so concurrent jobs
// building identical tags land on distinct image records in the shared
// buildkit namespace. Regression test for the E2E matrix race where one
// job's `docker build -t ephpm:dev` overwrote the other's and the loser
// served the wrong PHP version (ephpm/ephpm#67, #68).
func TestDockerBuildOptsToSolveOpt_ScopesTags(t *testing.T) {
	r := httptest.NewRequest("POST", "/build?t=ephpm:dev&t=ephpm-e2e:dev", nil)
	opt, err := dockerBuildOptsToSolveOpt(r, t.TempDir(), "ephemerd-github-ephpm-jobA")
	if err != nil {
		t.Fatalf("dockerBuildOptsToSolveOpt: %v", err)
	}
	if len(opt.Exports) != 1 {
		t.Fatalf("exports = %d, want 1", len(opt.Exports))
	}
	got := opt.Exports[0].Attrs["name"]
	want := "build.ephemerd.local/ephemerd-github-ephpm-joba/ephpm:dev," +
		"build.ephemerd.local/ephemerd-github-ephpm-joba/ephpm-e2e:dev"
	if got != want {
		t.Errorf("export name = %q, want %q", got, want)
	}
}

func TestPushLookupCandidates(t *testing.T) {
	got := pushLookupCandidates("jobA", "docker.io/ephpm:dev")
	want := []string{
		"build.ephemerd.local/joba/docker.io/ephpm:dev",
		"build.ephemerd.local/joba/ephpm:dev",
		"docker.io/ephpm:dev",
		"ephpm:dev",
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Scoped forms must come before unscoped: a job pushing its own build
	// must never accidentally resolve another path's unscoped record first.
	if !strings.HasPrefix(got[0], buildScopeRegistry+"/") || !strings.HasPrefix(got[1], buildScopeRegistry+"/") {
		t.Error("scoped candidates must be tried before unscoped fallbacks")
	}
}

func TestPushLookupCandidates_NoDockerPrefix(t *testing.T) {
	got := pushLookupCandidates("jobA", "ephpm:dev")
	want := []string{
		"build.ephemerd.local/joba/ephpm:dev",
		"ephpm:dev",
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDockerBuildOptsToSolveOpt_DistinctJobsDistinctNames is the actual
// race condition expressed as a property: same tag, different jobs, must
// produce different storage names.
func TestDockerBuildOptsToSolveOpt_DistinctJobsDistinctNames(t *testing.T) {
	mk := func(jobID string) string {
		r := httptest.NewRequest("POST", "/build?t=ephpm:dev", nil)
		opt, err := dockerBuildOptsToSolveOpt(r, t.TempDir(), jobID)
		if err != nil {
			t.Fatalf("dockerBuildOptsToSolveOpt(%s): %v", jobID, err)
		}
		return opt.Exports[0].Attrs["name"]
	}
	a := mk("ephemerd-github-ephpm-php84")
	b := mk("ephemerd-github-ephpm-php85")
	if a == b {
		t.Errorf("two jobs building the same tag produced the same storage name %q — the matrix race is back", a)
	}
}
