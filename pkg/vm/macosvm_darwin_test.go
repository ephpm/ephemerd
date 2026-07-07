//go:build darwin

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// TestMacOSRunnerSetupScript_AlwaysRefreshesRunner guards the fix for the
// stale-baked-runner bug: a runner baked into the base image goes stale after
// the pinned version is bumped (GitHub deprecates it, the broker refuses the
// JIT runner, and the VM job loops forever "queued"). The setup script must
// always overwrite the run dir with the freshly-provided runner and kill any
// already-running one first — never fall back to a baked copy when a fresh
// one is available.
func TestMacOSRunnerSetupScript_AlwaysRefreshesRunner(t *testing.T) {
	// Must kill any already-running (baked/LaunchDaemon) runner first.
	if !strings.Contains(macOSRunnerSetupScript, "pkill -f Runner.Listener") {
		t.Error("setup script must kill any pre-existing runner before starting the fresh one")
	}

	// Must remove the (possibly stale) run dir and re-copy from the fresh src.
	if !strings.Contains(macOSRunnerSetupScript, `rm -rf "$RUNNER_DIR"`) {
		t.Error("setup script must remove the run dir so a stale baked runner can't be reused")
	}
	if !strings.Contains(macOSRunnerSetupScript, `cp -R "$RUNNER_SRC" "$RUNNER_DIR"`) {
		t.Error("setup script must copy the freshly-provided runner into the run dir")
	}

	// The old skip-if-exists guard must never come back — it silently ran the
	// stale baked runner.
	if strings.Contains(macOSRunnerSetupScript, `[ ! -f "$RUNNER_DIR/run.sh" ]`) {
		t.Error("setup script must NOT skip the copy when a run.sh already exists (that reuses the stale baked runner)")
	}

	// The overwrite must be gated on the fresh source actually existing, so we
	// never delete the baked fallback when ephemerd provided nothing.
	if !strings.Contains(macOSRunnerSetupScript, `if [ -f "$RUNNER_SRC/run.sh" ]; then`) {
		t.Error("setup script must gate the overwrite on the fresh runner source existing")
	}
}

// TestMacOSRunnerSetupScript_TakesJITConfig confirms the template still has
// exactly one %s (the JIT config) so fmt.Sprintf wiring stays correct.
func TestMacOSRunnerSetupScript_TakesJITConfig(t *testing.T) {
	if got := strings.Count(macOSRunnerSetupScript, "%s"); got != 1 {
		t.Fatalf("setup script should contain exactly one %%s (JIT config), got %d", got)
	}
	out := fmt.Sprintf(macOSRunnerSetupScript, "JITDATA")
	if !strings.Contains(out, "--jitconfig 'JITDATA'") {
		t.Errorf("formatted script should embed the JIT config in the run.sh invocation:\n%s", out)
	}
}
