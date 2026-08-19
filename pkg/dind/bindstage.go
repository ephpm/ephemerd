package dind

import (
	"log/slog"
	"path/filepath"
)

// stagingDirName is the immediate child of the ephemerd data directory under
// which every job's bind staging mounts live: <data>/dind-binds/<job-id>/<n>.
//
// It is deliberately NOT under <data>/jobs/<job-id>/, which the runtime's
// orphan sweep os.RemoveAll's: recursively deleting a directory that has a
// bind mount inside it deletes the files visible THROUGH the mount, which here
// would be the runner's own rootfs.
const stagingDirName = "dind-binds"

// stagingRootDir is the parent of every job's staging directory.
func stagingRootDir(dataDir string) string {
	return filepath.Join(dataDir, stagingDirName)
}

// jobStagingDir is where one job's staged bind sources are published.
func jobStagingDir(dataDir, jobID string) string {
	return filepath.Join(stagingRootDir(dataDir), jobID)
}

// bindStager publishes a resolved bind source at a path ephemerd controls, so
// that the path the OCI spec carries has nothing in it a job can influence.
//
// WHY THIS LAYER EXISTS. Pinning the source to a descriptor (see bindPin) is
// only half a fix. The other half is getting that pinned inode into the spec,
// and the obvious route — putting "/proc/<ephemerd-pid>/fd/<n>" in
// ocispec.Mount.Source — does not work. runc does not re-resolve the string
// (its own error echoes it verbatim), but mount(2) rejects it:
//
//	error mounting "/proc/3543706/fd/3" to rootfs at "/marker":
//	mount src=/proc/3543706/fd/3, dst=/marker, dstFd=/proc/thread-self/fd/11,
//	flags=MS_BIND|MS_REC: invalid argument
//
// A bind source must live in the CALLER's mount namespace, and runc always
// has its own. That is unconditional — legitimate binds fail exactly the same
// way — so an fd handoff is a functional regression, not a fix. Verified
// against real runc 1.3.4 on kernel 6.8 and 6.18.
//
// What does work is to perform the bind in EPHEMERD's mount namespace, where
// the /proc/self/fd source resolves fine, onto a staging path ephemerd owns,
// and give runc that path. runc re-walks it — that is fine, because every
// component belongs to root and none of them is reachable from a job.
//
// Off Linux there is no dind daemon and no mount(2); see bindstage_other.go.
type bindStager interface {
	// stage publishes p and returns the path to put in the OCI spec. It
	// attaches the teardown to p, so releasing the pin releases the staging
	// mount. Failure must fail the bind — falling back to the resolved path
	// would reopen issue #125.
	stage(p *bindPin) (string, error)

	// teardown releases everything this stager published, including mounts
	// whose pins were lost. Called from Server.Stop.
	teardown()
}

// SweepStagedBinds removes bind staging mounts and directories left behind by
// a previous ephemerd process. STARTUP ONLY: it does not know which jobs are
// live, and unmounting a running job's staged bind would not break that job's
// already-running containers but would break any container it starts next.
//
// A hard kill (SIGKILL, panic, node reset) skips every teardown path, and the
// leaked mounts are not merely untidy: each one pins the runner container's
// rootfs mount, so containerd cannot delete the snapshot and the node
// accumulates undeletable snapshots until it runs out of disk.
func SweepStagedBinds(dataDir string, log *slog.Logger) {
	if dataDir == "" {
		return
	}
	sweepStagedBinds(stagingRootDir(dataDir), log)
}
