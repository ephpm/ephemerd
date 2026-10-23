// Package jobcgroup owns the per-job parent cgroup: the one cgroup that, on
// Linux, holds every container of one job.
//
// Layout (cgroup v2, cgroupfs driver — CgroupsPath "/<ns>/<id>"):
//
//	/sys/fs/cgroup/ephemerd/<runner-id>                 runner, no job limit
//	/sys/fs/cgroup/ephemerd-dind-<runner-id>/<cid>      dind siblings
//	/sys/fs/cgroup/ephemerd-dind-<runner-id>/runner     runner, WITH a job limit
//
// The dind siblings land under the job parent on their own: containerd's
// default spec sets CgroupsPath to "/<namespace>/<id>", and each job's dind
// namespace is "ephemerd-dind-<runner-id>". runc creates the parent with
// MkdirAll and only ever removes the leaf, which is why every finished job
// used to leave an empty ephemerd-dind-* directory behind (477 on ducks and
// 541 on sharks, 2026-10-23). Remove and SweepStale clean that up.
//
// With [runtime] memory_limit_mb set, the runner container is moved under the
// same parent (Dir(id)/runner) so the job's memory.max covers the runner and
// its siblings together — one budget per job. It cannot simply nest the
// siblings under the runner's own cgroup instead: cgroup v2's
// no-internal-processes rule forbids a cgroup that holds processes from
// having controller-enabled children.
package jobcgroup

// Root is the cgroup v2 mount ephemerd manages job cgroups under.
const Root = "/sys/fs/cgroup"

// Prefix is the name prefix of every job parent cgroup. It must equal
// dind.DindNamespacePrefix — the parent IS the cgroup containerd derives
// from the job's dind namespace — and dind's tests pin that.
const Prefix = "ephemerd-dind-"

// RunnerLeaf is the runner container's cgroup name inside the job parent
// when a job memory limit is configured. Sibling container ids are 64-hex
// digests, so it cannot collide with one.
const RunnerLeaf = "runner"

// Dir returns the job parent cgroup's path relative to the cgroupfs root,
// in the "/<name>" form OCI CgroupsPath takes.
func Dir(jobID string) string { return "/" + Prefix + jobID }

// RunnerPath is the runner container's CgroupsPath when the job has a
// memory budget.
func RunnerPath(jobID string) string { return Dir(jobID) + "/" + RunnerLeaf }

// Limits is the job's memory budget in bytes. Zero values mean no limit.
type Limits struct {
	MaxBytes  int64 // memory.max
	HighBytes int64 // memory.high; 0 = no soft limit
}

// Enabled reports whether the job has a memory budget at all. Without one
// nothing is created and the runner keeps its historical cgroup.
func (l Limits) Enabled() bool { return l.MaxBytes > 0 }
