package dind

import (
	"reflect"
	"testing"
	"time"
)

func TestBuildScopePrefix(t *testing.T) {
	tests := []struct {
		name, jobID, want string
	}{
		{"typical job id", "ephemerd-github-ephpm-agile_planck", "build.ephemerd.local/ephemerd-github-ephpm-agile_planck/"},
		{"uppercase is folded", "Ephemerd-GitHub-X", "build.ephemerd.local/ephemerd-github-x/"},
		// An empty prefix must never mean "match everything", or a
		// teardown would wipe the whole shared namespace.
		{"empty job id matches nothing", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BuildScopePrefix(tc.jobID); got != tc.want {
				t.Errorf("BuildScopePrefix(%q) = %q, want %q", tc.jobID, got, tc.want)
			}
		})
	}

	// The prefix must be consistent with the ref scoping used at build
	// time, or teardown silently matches nothing.
	const job = "ephemerd-github-ephpm-quick_mendel"
	ref := scopedBuildRef(job, "ephpm:dev")
	if prefix := BuildScopePrefix(job); len(ref) <= len(prefix) || ref[:len(prefix)] != prefix {
		t.Errorf("scopedBuildRef(%q, ...) = %q does not start with BuildScopePrefix = %q", job, ref, prefix)
	}
}

func TestBuildRecordJobID(t *testing.T) {
	tests := []struct {
		name, record, wantJob string
		wantOK                bool
	}{
		{
			name:    "job-scoped build export",
			record:  "build.ephemerd.local/ephemerd-github-ephpm-agile_planck/ephpm/ephemerd:runner-ci-linux-amd64",
			wantJob: "ephemerd-github-ephpm-agile_planck",
			wantOK:  true,
		},
		{
			name:    "scoped ref carrying its own registry qualifier",
			record:  "build.ephemerd.local/job-1/ghcr.io/owner/img:v1",
			wantJob: "job-1",
			wantOK:  true,
		},
		{
			// BuildKit's own cache records and anything else staged in
			// the namespace are NOT job garbage — never select them.
			name:   "unscoped record is not job output",
			record: "docker.io/library/alpine:3",
			wantOK: false,
		},
		{
			name:   "prefix with no tag component",
			record: "build.ephemerd.local/job-1/",
			wantOK: false,
		},
		{
			name:   "prefix with no job component",
			record: "build.ephemerd.local//img:v1",
			wantOK: false,
		},
		{
			name:   "bare registry name",
			record: "build.ephemerd.local",
			wantOK: false,
		},
		{
			name:   "empty",
			record: "",
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, ok := buildRecordJobID(tc.record)
			if ok != tc.wantOK {
				t.Fatalf("buildRecordJobID(%q) ok = %v, want %v", tc.record, ok, tc.wantOK)
			}
			if ok && job != tc.wantJob {
				t.Errorf("buildRecordJobID(%q) = %q, want %q", tc.record, job, tc.wantJob)
			}
		})
	}
}

func TestDeadBuildRecords(t *testing.T) {
	const (
		liveJob = "ephemerd-github-ephpm-live_job"
		dead1   = "ephemerd-github-ephpm-dead_one"
		dead2   = "ephemerd-github-ephpm-dead_two"
	)
	scoped := func(job, ref string) string { return BuildScopePrefix(job) + ref }

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	const grace = 15 * time.Minute
	old := now.Add(-time.Hour)     // safely past the grace window
	young := now.Add(-time.Minute) // inside the grace window
	rec := func(name string, at time.Time) BuildRecord { return BuildRecord{Name: name, UpdatedAt: at} }

	tests := []struct {
		name    string
		records []BuildRecord
		live    map[string]struct{}
		want    []string
	}{
		{
			name: "selects records of dead jobs only",
			records: []BuildRecord{
				rec(scoped(liveJob, "ephpm:dev"), old),
				rec(scoped(dead1, "ephpm:dev"), old),
				rec(scoped(dead2, "ephpm:dev"), old),
			},
			live: map[string]struct{}{liveJob: {}},
			want: []string{scoped(dead1, "ephpm:dev"), scoped(dead2, "ephpm:dev")},
		},
		{
			// scopedBuildRef lowercases the job ID; the live set comes
			// from containerd container IDs, which may not be. Matching
			// must be case-insensitive or a live job's build output gets
			// deleted mid-job.
			name:    "live job matching is case-insensitive",
			records: []BuildRecord{rec(scoped("Ephemerd-Mixed-Case", "img:v1"), old)},
			live:    map[string]struct{}{"Ephemerd-Mixed-Case": {}},
			want:    nil,
		},
		{
			// The live set is captured BEFORE the record listing, so a
			// record written moments ago may belong to a job that started
			// in between — it must survive this pass even though its job
			// is not in the (stale) live set. A later tick collects it
			// once the job is provably gone.
			name:    "never selects a record younger than the grace",
			records: []BuildRecord{rec(scoped(dead1, "ephpm:dev"), young)},
			live:    nil,
			want:    nil,
		},
		{
			// An unknown age can never satisfy "older than the grace" —
			// fail safe, exactly like the dead-container reaper.
			name:    "never selects a record with no timestamp",
			records: []BuildRecord{rec(scoped(dead1, "ephpm:dev"), time.Time{})},
			live:    nil,
			want:    nil,
		},
		{
			// BuildKit's own cache records live in the same namespace
			// and are owned by its bbolt DB — deleting them behind its
			// back leaves snapshots pinned and corrupts the index.
			name: "never selects records that are not job-scoped",
			records: []BuildRecord{
				rec("docker.io/library/alpine:3", old),
				rec("sha256:deadbeef", old),
			},
			live: nil,
			want: nil,
		},
		{
			name: "no live jobs means every old scoped record is dead",
			records: []BuildRecord{
				rec(scoped(dead1, "a:1"), old),
				rec(scoped(dead2, "b:2"), old),
			},
			live: nil,
			want: []string{scoped(dead1, "a:1"), scoped(dead2, "b:2")},
		},
		{
			name:    "empty input",
			records: nil,
			live:    map[string]struct{}{liveJob: {}},
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeadBuildRecords(tc.records, tc.live, now, grace)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DeadBuildRecords() = %v, want %v", got, tc.want)
			}
		})
	}
}
