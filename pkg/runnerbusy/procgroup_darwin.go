//go:build darwin

package runnerbusy

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
)

// ProcessGroupBusy reports whether a native macOS runner — one that runs
// as a plain host process group rather than inside a container or VM — is
// executing a job.
//
// The runner is started as a process-group leader (pgid == the listener's
// pid), and every process it forks, including the per-job worker, inherits
// that group. So "does this process group contain a Runner.Worker" is the
// same question the container probes ask, expressed in the units macOS
// gives us.
//
// pgrep exit codes: 0 = matched, 1 = no match, anything else = the query
// itself failed. Only the first two are answers; the rest is Unknown.
func ProcessGroupBusy(ctx context.Context, pgid int) (State, error) {
	if pgid <= 0 {
		return Unknown, errors.New("native runner has no process group yet")
	}
	// -g restricts the search to the runner's process group; -x demands an
	// exact process-name match so a step that happens to mention the
	// worker in its argv cannot fake a busy verdict.
	cmd := exec.CommandContext(ctx, "pgrep", "-g", strconv.Itoa(pgid), "-x", "Runner.Worker")
	out, err := cmd.Output()
	if err == nil {
		if len(out) == 0 {
			return Unknown, errors.New("pgrep matched but returned no pids")
		}
		return Busy, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return Idle, nil
	}
	return Unknown, fmt.Errorf("pgrep for a worker in process group %d: %w", pgid, err)
}
