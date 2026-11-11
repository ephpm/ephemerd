package runtime

import (
	"path/filepath"
	"testing"
)

type fakeOverride struct {
	dir string
	ok  bool
	got string
}

func (f *fakeOverride) OfficialRunnerOverride(image string) (string, bool) {
	f.got = image
	return f.dir, f.ok
}

func TestPlanRunnerMount(t *testing.T) {
	const cfgDir, cfgMount = "/data/runners/2.337.0-linux", "/actions-runner"
	const official = "ephpm/ephemerd:runner-ci-linux-amd64"
	engaged := &fakeOverride{dir: "/data/runners/verified/extracted/2.338.2-linux", ok: true}
	idle := &fakeOverride{}

	// Historical behaviour, untouched: custom image mounts the configured
	// runner; official image on Linux mounts nothing; Windows always mounts.
	runnersRoot := filepath.Dir(cfgDir)
	if p := planRunnerMount(true, "linux", cfgDir, cfgMount, nil, "foo/bar:1"); !p.needsMount || p.dir != cfgDir || p.mount != cfgMount || p.jobRoot != runnersRoot || p.override {
		t.Fatalf("custom: %+v", p)
	}
	if p := planRunnerMount(false, "linux", cfgDir, cfgMount, nil, official); p.needsMount {
		t.Fatalf("official, no source: %+v", p)
	}
	if p := planRunnerMount(false, "linux", cfgDir, cfgMount, idle, official); p.needsMount {
		t.Fatalf("official, idle source: %+v", p)
	}
	if p := planRunnerMount(false, "windows", cfgDir, cfgMount, engaged, "mcr.microsoft.com/windows/servercore:ltsc2022"); !p.needsMount || p.override || p.mount != cfgMount {
		t.Fatalf("windows: %+v", p)
	}
	if p := planRunnerMount(true, "linux", "", "", nil, "foo/bar:1"); p.needsMount {
		t.Fatalf("custom without configured runner: %+v", p)
	}

	// The new case: official image on Linux with an engaged override mounts
	// the override tree at /home/runner.
	p := planRunnerMount(false, "linux", cfgDir, cfgMount, engaged, official)
	if !p.needsMount || !p.override || p.dir != engaged.dir || p.mount != officialRunnerRoot {
		t.Fatalf("engaged: %+v", p)
	}
	if engaged.got != official {
		t.Fatalf("source asked about %q", engaged.got)
	}
	// The per-job writable tree stays under the configured runners dir,
	// never beside the override lowerdir inside the verified store (which
	// Prune would delete and the orphan sweep would never see).
	if p.jobRoot != runnersRoot {
		t.Fatalf("engaged jobRoot = %q, want %q", p.jobRoot, runnersRoot)
	}
	if p := planRunnerMount(false, "linux", "", "", engaged, official); p.jobRoot != filepath.Dir(engaged.dir) {
		t.Fatalf("engaged without configured runner: jobRoot = %q", p.jobRoot)
	}
	// A custom image is never masked even with an override engaged: it
	// already gets the configured runner at /actions-runner.
	if p := planRunnerMount(true, "linux", cfgDir, cfgMount, engaged, "foo/bar:1"); p.override || p.mount != cfgMount {
		t.Fatalf("custom with engaged source: %+v", p)
	}
}
