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

// fakeSource implements both seams: the official-image override and the
// verified-cache redirect for mounted paths.
type fakeSource struct {
	fakeOverride
	mounted string
	mok     bool
}

func (f *fakeSource) MountedRunnerDir() (string, bool) { return f.mounted, f.mok }

func TestPlanRunnerMount_MountedRunnerSource(t *testing.T) {
	const cfgDir, cfgMount = "/data/runners/2.338.2-linux", "/actions-runner"
	const cached = "/data/runners/verified/extracted/2.339.0-linux"
	runnersRoot := filepath.Dir(cfgDir)

	// Cache wins for a custom image and on Windows: the lowerdir / copy
	// source is the cached tree, the per-job tree stays under the
	// configured runners dir, and it is not an "override" (no alert).
	src := &fakeSource{mounted: cached, mok: true}
	for _, tc := range []struct {
		name   string
		custom bool
		goos   string
		image  string
	}{
		{"custom linux", true, "linux", "foo/bar:1"},
		{"windows", false, "windows", "mcr.microsoft.com/windows/servercore:ltsc2022"},
	} {
		p := planRunnerMount(tc.custom, tc.goos, cfgDir, cfgMount, src, tc.image)
		if !p.needsMount || p.dir != cached || p.mount != cfgMount || p.jobRoot != runnersRoot || p.override {
			t.Fatalf("%s: %+v", tc.name, p)
		}
	}
	// Embedded wins (ok=false): byte-for-byte the historical plan.
	src.mok = false
	if p := planRunnerMount(true, "linux", cfgDir, cfgMount, src, "foo/bar:1"); p.dir != cfgDir || p.jobRoot != runnersRoot {
		t.Fatalf("embedded wins: %+v", p)
	}
	// A source without the optional interface changes nothing.
	if p := planRunnerMount(true, "linux", cfgDir, cfgMount, &fakeOverride{}, "foo/bar:1"); p.dir != cfgDir {
		t.Fatalf("plain source: %+v", p)
	}
	// The cache never masks an official image through THIS seam; that is
	// the override's job (and it was not engaged here).
	src.mok = true
	if p := planRunnerMount(false, "linux", cfgDir, cfgMount, src, "ghcr.io/actions/actions-runner:latest"); p.needsMount {
		t.Fatalf("official image with only a mounted source: %+v", p)
	}
}
