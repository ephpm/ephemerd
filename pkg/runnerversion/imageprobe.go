package runnerversion

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/ephpm/ephemerd/pkg/runner"
)

// The dominant path on this fleet is NOT the embedded tarball: Linux jobs
// run the runner baked into the runner-ci-linux image (FROM
// ghcr.io/actions/actions-runner), which the runtime treats as "official"
// and runs from /home/runner with no runner mount at all. That runner can
// fall below GitHub's floor independently of the embedded pin (2026-11-10:
// image at 2.336.0, every Linux job refused). So the subsystem must be able
// to read the version of a runner TREE — the image's /home/runner, the
// embedded extraction, or a cache entry — not just a tarball's filename.
//
// The .NET deps file the runner ships records the Listener's project
// version: bin/Runner.Listener.deps.json contains "Runner.Listener/2.337.0"
// (verified against the 2.337.0 win-x64 zip; the Linux tree is the same
// layout). It is the one version marker present in an unconfigured tree —
// .runner only exists after config.sh, and the image never runs that.

// ListenerDepsFile is the file ProbeRunnerTree reads, relative to the root.
var ListenerDepsFile = filepath.Join("bin", "Runner.Listener.deps.json")

var listenerVersionRe = regexp.MustCompile(`"Runner\.Listener/(\d+\.\d+\.\d+)"`)

// ErrNoRunnerTree means the directory does not look like a runner tree.
var ErrNoRunnerTree = errors.New("not a runner tree")

// TreeInfo describes a runner tree on disk.
type TreeInfo struct {
	Dir     string
	Version string
	Parts   []int
	// UID/GID own the tree's root directory. An overlay mounted over the
	// image's /home/runner must present the same owner, or the image's
	// unprivileged `runner` user cannot create _work/. -1 when unknown
	// (non-Unix host).
	UID, GID int
}

// ParseListenerDepsVersion extracts the Listener version from a
// Runner.Listener.deps.json document.
func ParseListenerDepsVersion(data []byte) (string, error) {
	m := listenerVersionRe.FindSubmatch(data)
	if m == nil {
		return "", fmt.Errorf("%w: no Runner.Listener/X.Y.Z entry in deps.json", ErrNoRunnerTree)
	}
	return string(m[1]), nil
}

// ProbeRunnerTree reads the runner version and ownership of a tree rooted
// at dir. It works on any runner tree: the embedded extraction, a verified
// cache entry, or an image's /home/runner mounted read-only.
func ProbeRunnerTree(dir string) (TreeInfo, error) {
	info := TreeInfo{Dir: dir, UID: -1, GID: -1}
	data, err := os.ReadFile(filepath.Join(dir, ListenerDepsFile))
	if err != nil {
		return info, fmt.Errorf("%w: %w", ErrNoRunnerTree, err)
	}
	v, err := ParseListenerDepsVersion(data)
	if err != nil {
		return info, err
	}
	info.Version = v
	info.Parts = runner.ParseVersionString(v)
	st, err := os.Stat(dir)
	if err != nil {
		return info, err
	}
	info.UID, info.GID = ownerOf(st)
	return info, nil
}
