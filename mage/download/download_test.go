package download

import (
	"strings"
	"testing"
)

// The signed runner manifest is built from this table, so the table must
// describe exactly the five archives the fleet ships, for RunnerVersion,
// with hashes that look like hashes.
func TestRunnerArtifacts(t *testing.T) {
	arts := RunnerArtifacts()
	want := map[string]string{
		"linux-x64":   "actions-runner-linux-x64-" + RunnerVersion + ".tar.gz",
		"linux-arm64": "actions-runner-linux-arm64-" + RunnerVersion + ".tar.gz",
		"win-x64":     "actions-runner-win-x64-" + RunnerVersion + ".zip",
		"osx-x64":     "actions-runner-osx-x64-" + RunnerVersion + ".tar.gz",
		"osx-arm64":   "actions-runner-osx-arm64-" + RunnerVersion + ".tar.gz",
	}
	if len(arts) != len(want) {
		t.Fatalf("got %d artifacts, want %d: %+v", len(arts), len(want), arts)
	}
	for _, a := range arts {
		if want[a.Platform] != a.Filename {
			t.Errorf("platform %s: filename %q, want %q", a.Platform, a.Filename, want[a.Platform])
		}
		if a.URL != RunnerArchiveURL(a.Filename) || !strings.HasSuffix(a.URL, "/v"+RunnerVersion+"/"+a.Filename) {
			t.Errorf("platform %s: url %q", a.Platform, a.URL)
		}
		if len(a.SHA256) != 64 || strings.ToLower(a.SHA256) != a.SHA256 {
			t.Errorf("platform %s: sha %q is not 64 lower-case hex chars", a.Platform, a.SHA256)
		}
		if sha, ok := pinnedFor(a.Filename); !ok || sha != a.SHA256 {
			t.Errorf("platform %s: artifact sha diverges from pinnedSHA256", a.Platform)
		}
	}
	// Sorted by platform so the manifest is deterministic.
	for i := 1; i < len(arts); i++ {
		if arts[i-1].Platform > arts[i].Platform {
			t.Fatalf("artifacts not sorted: %s before %s", arts[i-1].Platform, arts[i].Platform)
		}
	}
}

// RunnerFloor must parse and must not exceed RunnerVersion; the manifest
// validator enforces the same, but a build should fail before CI does.
func TestRunnerFloorNotAboveVersion(t *testing.T) {
	parse := func(v string) [3]int {
		var out [3]int
		parts := strings.Split(v, ".")
		if len(parts) != 3 {
			t.Fatalf("%q is not X.Y.Z", v)
		}
		for i, p := range parts {
			n := 0
			for _, c := range p {
				if c < '0' || c > '9' {
					t.Fatalf("%q is not numeric", v)
				}
				n = n*10 + int(c-'0')
			}
			out[i] = n
		}
		return out
	}
	f, v := parse(RunnerFloor), parse(RunnerVersion)
	for i := range f {
		if f[i] < v[i] {
			return
		}
		if f[i] > v[i] {
			t.Fatalf("RunnerFloor %s is above RunnerVersion %s", RunnerFloor, RunnerVersion)
		}
	}
}
