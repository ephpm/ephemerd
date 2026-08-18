//go:build !linux

package dind

import (
	"log/slog"
	"os"
)

// newBindStager returns the off-Linux stager. There is no mount(2) here and,
// more to the point, no dind daemon: sibling containers are created by the
// linux build of ephemerd — directly on a Linux node, or inside the managed
// Linux VM on a Windows/macOS node — and bind translation is not wired into
// the Windows or macOS container paths at all (see pkg/runtime/runtime.go and
// pkg/dind/containers.go, where the Windows sibling path builds its mounts
// separately).
//
// So this stager hands back the resolved path unchanged. That is the
// pre-#125 behaviour, and it is safe here only because nothing on these
// platforms feeds a job-controlled source into it.
func newBindStager(dataDir, jobID string, log *slog.Logger) bindStager {
	return passthroughStager{}
}

type passthroughStager struct{}

func (passthroughStager) stage(p *bindPin) (string, error) {
	return p.Logical(), nil
}

func (passthroughStager) teardown() {}

// sweepStagedBinds has nothing to unmount off Linux; remove the directory if a
// Linux-side data dir ever gets inspected from a dev host.
func sweepStagedBinds(root string, log *slog.Logger) {
	_ = os.RemoveAll(root)
}
