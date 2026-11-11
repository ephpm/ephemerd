package runner

import (
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// This file is the additive seam for pkg/runnerversion (the self-healing
// runner subsystem, docs/arch/self-healing-runner.md). It exports the
// version-comparison and archive helpers that runner.go keeps unexported, so
// the selector that chooses between the embedded runner and a verified
// cached one uses the SAME ordering findTarball uses. Nothing in runner.go
// is changed; this file only wraps.

// ParseVersion extracts the numeric version components from a runner
// archive name like "actions-runner-linux-x64-2.338.2.tar.gz" -> [2 338 2].
// Names it can't parse return nil, which sorts lowest.
func ParseVersion(name string) []int {
	return parseRunnerVersion(name)
}

// ParseVersionString parses a bare version like "2.338.2" -> [2 338 2].
// Returns nil when any component is not a number.
func ParseVersionString(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return nil
	}
	return parseRunnerVersion("x-" + v)
}

// CompareVersions returns >0 if a>b, <0 if a<b, 0 if equal, comparing
// numeric components left to right (a missing component counts as 0).
func CompareVersions(a, b []int) int {
	return compareRunnerVersions(a, b)
}

// EmbeddedArchive opens the embedded runner archive that Extract would
// use for this OS (the highest embedded version). The returned name is the
// archive's base filename (e.g. "actions-runner-linux-x64-2.337.0.tar.gz");
// the caller closes the file.
func (m *Manager) EmbeddedArchive() (string, fs.File, error) {
	path, err := m.findTarball()
	if err != nil {
		return "", nil, err
	}
	f, err := runnerFS.Open(path)
	if err != nil {
		return "", nil, fmt.Errorf("opening embedded runner: %w", err)
	}
	return strings.TrimPrefix(path, "embed/"), f, nil
}

// ExtractArchive extracts a runner archive stream into dest. The format is
// chosen by name: ".zip" for the Windows runner, gzip'd tar otherwise. Path
// traversal is rejected exactly as for the embedded runner.
func ExtractArchive(r io.Reader, name, dest string) error {
	if strings.HasSuffix(name, ".zip") {
		return extractZipFromReader(r, dest)
	}
	return extractTarGz(r, dest)
}
