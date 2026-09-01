//go:build linux

package runnerbusy

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
)

// ContainerBusy reports whether the runner inside a containerd-managed
// Linux container is executing a job.
//
// runc leaves container processes visible in the host's PID namespace, so
// the PIDs containerd reports are readable under /proc on the host. For
// each one we read argv[0] (falling back to the kernel's comm) and look
// for the runner's worker child.
//
// Failure modes all resolve to Unknown, never Idle:
//
//   - the task query fails (shim gone, containerd restarting)
//   - the task reports no processes at all, which is indistinguishable
//     from a listing that raced container teardown
//   - every /proc read failed, which means we are not in a position to
//     see the container's processes (foreign PID namespace, hardened
//     /proc) rather than that the container is quiet
func ContainerBusy(ctx context.Context, t ContainerTask, log *slog.Logger) (State, error) {
	pids, err := t.Pids(ctx)
	if err != nil {
		return Unknown, fmt.Errorf("listing processes in container %s: %w", t.ID(), err)
	}
	if len(pids) == 0 {
		return Unknown, fmt.Errorf("container %s reported no processes", t.ID())
	}

	read := 0
	for _, p := range pids {
		name, err := processName(int(p.Pid))
		if err != nil {
			// The process exited between the listing and the read. That
			// is normal churn in a busy container, not a probe failure —
			// keep going and judge on what we could read.
			log.Debug("busy probe could not read a container process", "container", t.ID(), "pid", p.Pid, "error", err)
			continue
		}
		read++
		if IsWorkerProcess(name) {
			return Busy, nil
		}
	}
	if read == 0 {
		return Unknown, fmt.Errorf("container %s: none of its %d processes were readable under /proc", t.ID(), len(pids))
	}
	return Idle, nil
}

// processName returns argv[0] for a host PID, falling back to the kernel's
// comm when the cmdline is empty (kernel threads, or a process caught
// mid-exec). "Runner.Worker" is 13 bytes, so it survives comm's 15-byte
// truncation intact.
func processName(pid int) (string, error) {
	dir := "/proc/" + strconv.Itoa(pid)
	if b, err := os.ReadFile(dir + "/cmdline"); err == nil {
		if argv0, _, _ := bytes.Cut(b, []byte{0}); len(argv0) > 0 {
			return string(argv0), nil
		}
	}
	b, err := os.ReadFile(dir + "/comm")
	if err != nil {
		return "", fmt.Errorf("reading name of pid %d: %w", pid, err)
	}
	return string(bytes.TrimSpace(b)), nil
}
