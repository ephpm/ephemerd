// Package runnerbusy answers one question from ground truth rather than
// inference: is a GitHub Actions runner executing a job RIGHT NOW?
//
// ephemerd otherwise only learns that a runner is busy by processing an
// `in_progress` webhook. That is inference ABOUT busy-ness, not an
// observation of it: anything that delays, drops or reorders a delivery —
// and a burst of same-label jobs makes all three likely — leaves a runner
// that is genuinely executing a build looking idle in the scheduler's
// ledger. Teardown decisions taken on that belief kill live builds.
//
// The signal used here is the actions-runner's own process model. The
// listener process (`Runner.Listener`) runs for the whole life of the
// runner, so "a runner process exists" says nothing. The listener forks a
// `Runner.Worker` child ONLY while a job is executing, and reaps it when
// the job ends. "A worker exists" is therefore equivalent to "a job is
// running", and ephemerd — which owns the container, VM or process the
// runner lives in — can observe it locally: no network, no API budget,
// and immune to a missed or reordered webhook.
//
// Every probe returns a State. Unknown is NOT idle. Callers MUST treat
// Unknown as "possibly busy" and fail safe; a probe that cannot answer
// never reports Idle.
package runnerbusy

import (
	"context"
	"errors"
	"strings"

	"github.com/containerd/containerd/v2/client"
)

// State is a probe's verdict about a runner.
type State int

const (
	// Unknown means the probe could not determine the runner's state:
	// the platform has no local probe, the runtime refused the query, or
	// the answer was empty in a way that cannot be distinguished from a
	// failure. Callers must treat it as "possibly busy".
	Unknown State = iota

	// Idle means the probe positively observed that no job is executing:
	// the runner's process list was read successfully and contained no
	// worker. Only this verdict makes teardown safe.
	Idle

	// Busy means the probe positively observed a worker process, i.e. a
	// job is executing right now. Destroying the runner would kill it.
	Busy
)

func (s State) String() string {
	switch s {
	case Idle:
		return "idle"
	case Busy:
		return "busy"
	default:
		return "unknown"
	}
}

// ErrUnsupported is returned by probes that have no implementation on the
// running platform. It is an explicit "I cannot answer" — never an
// implicit "not busy".
var ErrUnsupported = errors.New("runnerbusy: no local probe on this platform")

// workerProcess is the actions-runner's per-job child process. It exists
// for exactly as long as a job is executing.
const workerProcess = "runner.worker"

// IsWorkerProcess reports whether a process identifier names the
// actions-runner's job worker.
//
// The argument is whatever the platform's process listing yields for a
// process: argv[0] on Linux (an absolute path such as
// /home/runner/bin/Runner.Worker), /proc/<pid>/comm as a fallback, or the
// HCS ImageName on Windows (Runner.Worker.exe). Matching is on the base
// name, case-insensitively, with a .exe suffix stripped, so one rule
// covers all three.
//
// Runner.Listener is deliberately NOT matched: it is alive for the whole
// life of the runner, including while the runner sits idle waiting for a
// job, so matching it would make every runner look permanently busy.
func IsWorkerProcess(name string) bool {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToLower(name)
	name = strings.TrimSuffix(name, ".exe")
	return name == workerProcess
}

// ContainerTask is the slice of containerd's client.Task that the
// container probe needs. Declared as an interface so the probe can be
// unit-tested without a containerd daemon.
//
// ID returns the container ID (the init task's ID is the container's).
// Pids lists the processes in the container.
type ContainerTask interface {
	ID() string
	Pids(context.Context) ([]client.ProcessInfo, error)
}
