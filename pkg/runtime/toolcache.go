package runtime

import (
	"context"
	"strings"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// WindowsToolCache is the RUNNER_TOOL_CACHE directory for Windows job
// containers.
//
// The GitHub runner resolves its tool cache from, in order,
// RUNNER_TOOL_CACHE, RUNNER_TOOLSDIRECTORY, AGENT_TOOLSDIRECTORY,
// agent.ToolsDirectory, and only then falls back to <runner root>\_work\_tool
// (Runner.Common, HostContext.GetDirectory, WellKnownDirectory.Tools). Every
// action that caches a toolchain — actions/setup-go, setup-node, setup-python —
// looks there and nowhere else. PATH is irrelevant to them: an image that
// installs Go and puts it on PATH still makes setup-go download and extract a
// second copy.
//
// The default is unusable for us. On Windows the whole runner root is a
// per-job copy of the host runner directory that ephemerd maps into the
// container (see withRunnerMount and copyDirForJob); the runner creates _work
// inside it. So <runner root>\_work\_tool is host storage reached over a VSMB
// share into the Hyper-V utility VM, and it is created fresh per job. Two
// consequences: anything we bake into the image under C:\actions-runner is
// shadowed by the mapped directory, and small-file writes there are slow
// enough that extracting the ~15,000-file Go toolchain zip blows past
// actions/setup-go's 8 minute timeout. Pointing the tool cache at a path
// inside the image fixes both — the contents survive because nothing is
// mounted over them, and a cache hit means no extraction at all.
//
// C:\hostedtoolcache mirrors the GitHub-hosted Windows images and the
// /opt/hostedtoolcache we already hand to Gitea/Forgejo runners in
// pkg/forgerunner.BuildEnv. images/runner-ci-windows/Dockerfile seeds it; keep
// the two in sync.
const WindowsToolCache = `C:\hostedtoolcache`

// withDefaultEnv sets key=value in the container spec only if nothing has set
// it already.
//
// It must be applied after oci.WithImageConfig and oci.WithEnv so that both an
// image's own ENV and a job's explicit environment win over the default. That
// ordering is the point: a user bringing a custom Windows image with its own
// tool cache location keeps it, and we only fill in the gap that would
// otherwise leave the runner on its slow default.
func withDefaultEnv(key, value string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Process == nil {
			s.Process = &ocispec.Process{}
		}
		prefix := key + "="
		for _, kv := range s.Process.Env {
			if strings.HasPrefix(kv, prefix) {
				return nil
			}
		}
		s.Process.Env = append(s.Process.Env, prefix+value)
		return nil
	}
}
