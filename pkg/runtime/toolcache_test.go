package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// The whole point of the default is that it fills a gap. If it does not get
// added when nothing else set it, every Windows job falls back to
// <runner root>\_work\_tool — the mapped host directory — and actions/setup-go
// re-extracts a 15,000-file toolchain over VSMB on every single run.
func TestWithDefaultEnv_SetsWhenAbsent(t *testing.T) {
	spec := &oci.Spec{Process: &ocispec.Process{Env: []string{"PATH=C:\\Windows"}}}
	applyOpts(t, spec, []oci.SpecOpts{withDefaultEnv("RUNNER_TOOL_CACHE", WindowsToolCache)})

	want := "RUNNER_TOOL_CACHE=" + WindowsToolCache
	if !slices.Contains(spec.Process.Env, want) {
		t.Errorf("env = %v, want it to contain %q", spec.Process.Env, want)
	}
}

// A user's custom Windows image may ship its own tool cache somewhere else.
// withDefaultEnv is applied after oci.WithImageConfig and oci.WithEnv precisely
// so the image and the job win; if that ever inverts, we would silently point
// those runners at a directory that does not exist.
func TestWithDefaultEnv_DoesNotOverrideExisting(t *testing.T) {
	spec := &oci.Spec{Process: &ocispec.Process{
		Env: []string{"RUNNER_TOOL_CACHE=C:\\custom\\tools"},
	}}
	applyOpts(t, spec, []oci.SpecOpts{withDefaultEnv("RUNNER_TOOL_CACHE", WindowsToolCache)})

	if len(spec.Process.Env) != 1 {
		t.Fatalf("env = %v, want the single pre-existing entry", spec.Process.Env)
	}
	if spec.Process.Env[0] != "RUNNER_TOOL_CACHE=C:\\custom\\tools" {
		t.Errorf("env[0] = %q, want the image's own value preserved", spec.Process.Env[0])
	}
}

// Matching on the bare name rather than name+"=" would treat
// RUNNER_TOOL_CACHE_SOMETHING as "already set" and skip the default.
func TestWithDefaultEnv_IgnoresPrefixCollision(t *testing.T) {
	spec := &oci.Spec{Process: &ocispec.Process{
		Env: []string{"RUNNER_TOOL_CACHE_MODE=readonly"},
	}}
	applyOpts(t, spec, []oci.SpecOpts{withDefaultEnv("RUNNER_TOOL_CACHE", WindowsToolCache)})

	want := "RUNNER_TOOL_CACHE=" + WindowsToolCache
	if !slices.Contains(spec.Process.Env, want) {
		t.Errorf("env = %v, want it to contain %q", spec.Process.Env, want)
	}
}

func TestWithDefaultEnv_ToleratesNilProcess(t *testing.T) {
	spec := &oci.Spec{}
	applyOpts(t, spec, []oci.SpecOpts{withDefaultEnv("RUNNER_TOOL_CACHE", WindowsToolCache)})

	if spec.Process == nil {
		t.Fatal("Process still nil after opts")
	}
	want := "RUNNER_TOOL_CACHE=" + WindowsToolCache
	if !slices.Contains(spec.Process.Env, want) {
		t.Errorf("env = %v, want it to contain %q", spec.Process.Env, want)
	}
}

// The tool cache path is a contract between this package and the Windows
// runner image: ephemerd points RUNNER_TOOL_CACHE at it, the Dockerfile is what
// puts anything there. If they drift, RUNNER_TOOL_CACHE names an empty
// directory and setup-go silently goes back to downloading and extracting —
// which is a nine-minute timeout, not a visible error.
func TestWindowsToolCache_MatchesRunnerImage(t *testing.T) {
	path := filepath.Join("..", "..", "images", "runner-ci-windows", "Dockerfile")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	dockerfile := string(b)

	if want := `ENV RUNNER_TOOL_CACHE="` + WindowsToolCache + `"`; !strings.Contains(dockerfile, want) {
		t.Errorf("%s does not contain %q", path, want)
	}
	// The layout actions/toolkit's tool-cache defines and setup-go consumes:
	// <cache>\<tool>\<x.y.z>\<arch> plus a sibling <arch>.complete marker file.
	// Without the marker, tc.find() reports a miss even though the toolchain
	// is sitting right there.
	for _, want := range []string{
		WindowsToolCache + `\go\$v\x64"`,
		WindowsToolCache + `\go\$v\x64.complete"`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("%s does not seed %q", path, want)
		}
	}
}
