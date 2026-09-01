//go:build windows

package runnerbusy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Microsoft/hcsshim"
)

// hcsProcessLister is the minimal subset of hcsshim.Container the Windows
// probe needs. Injectable so the probe can be tested without a live
// compute system.
type hcsProcessLister interface {
	ProcessList() ([]hcsshim.ProcessListItem, error)
	Close() error
}

// openCompute is swapped out in tests.
var openCompute = func(id string) (hcsProcessLister, error) {
	c, err := hcsshim.OpenContainer(id)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ContainerBusy reports whether the runner inside a Windows container is
// executing a job.
//
// Windows runner containers are Hyper-V isolated, so the runner's
// processes live in a utility VM and are NOT visible in the host's
// process table — the Linux probe's /proc walk has no host-side
// equivalent here, and inspecting the host would silently report "no
// worker" for every job. The Host Compute Service is the supported way
// in: it proxies a process listing out of the guest through the GCS, for
// both process-isolated and Hyper-V isolated containers, and reports each
// process's image name. This is the same handle pkg/metrics already opens
// per container for resource sampling.
//
// Same fail-safe rule as the Linux probe: anything we cannot read is
// Unknown, and an empty listing (which HCS also returns for a compute
// system that has gone away underneath us) is Unknown rather than Idle.
//
// The hcsshim calls take no context, so the probe runs them on a
// goroutine and selects on ctx: without this, the caller's
// busyProbeTimeout was simply not enforced on Windows, and a dead
// shim/GCS wedged the probe (and the sweep serialized behind it)
// indefinitely. On ctx expiry the goroutine is abandoned — it parks
// inside HCS until that call eventually errors — and the probe reports
// Unknown, which is the fail-safe veto.
func ContainerBusy(ctx context.Context, t ContainerTask, log *slog.Logger) (State, error) {
	id := t.ID()

	type result struct {
		state State
		err   error
	}
	ch := make(chan result, 1) // buffered: a late probe result must not leak the goroutine
	go func() {
		st, err := containerBusyHCS(id, log)
		ch <- result{st, err}
	}()

	select {
	case r := <-ch:
		return r.state, r.err
	case <-ctx.Done():
		return Unknown, fmt.Errorf("busy probe for compute system %q: %w", id, ctx.Err())
	}
}

// containerBusyHCS is the uncancellable HCS half of ContainerBusy.
func containerBusyHCS(id string, log *slog.Logger) (State, error) {
	c, err := openCompute(id)
	if err != nil {
		return Unknown, fmt.Errorf("opening compute system %q: %w", id, err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			log.Debug("closing compute system after busy probe", "container", id, "error", err)
		}
	}()

	list, err := c.ProcessList()
	if err != nil {
		return Unknown, fmt.Errorf("listing processes in compute system %q: %w", id, err)
	}
	if len(list) == 0 {
		return Unknown, fmt.Errorf("compute system %q reported no processes", id)
	}
	for _, p := range list {
		if IsWorkerProcess(p.ImageName) {
			return Busy, nil
		}
	}
	return Idle, nil
}
