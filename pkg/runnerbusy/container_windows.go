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
func ContainerBusy(_ context.Context, t ContainerTask, log *slog.Logger) (State, error) {
	id := t.ID()
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
