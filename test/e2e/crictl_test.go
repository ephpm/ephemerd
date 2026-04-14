//go:build e2e && privileged

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	ctdpkg "github.com/ephpm/ephemerd/pkg/containerd"
)

// crictl subprocess hook env vars — read by TestMain in e2e_test.go.
// We re-exec the test binary so crictl.Main() (which may os.Exit via
// logrus.Fatal) cannot tear down the parent test process.
const (
	crictlSocketEnv = "EPHEMERD_E2E_CRICTL_SOCKET"
	crictlArgsEnv   = "EPHEMERD_E2E_CRICTL_ARGS"
)

// runCrictl spawns the test binary as a crictl child against the shared
// containerd socket and returns stdout, stderr, and exit code.
func runCrictl(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating test binary: %v", err)
	}

	socket := ctdpkg.SocketPath(sharedDataDir)

	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		crictlSocketEnv+"="+socket,
		crictlArgsEnv+"="+strings.Join(args, "\x00"),
	)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	exitCode = 0
	if ee, ok := runErr.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	} else if runErr != nil {
		t.Fatalf("running crictl subprocess: %v", runErr)
	}

	return outBuf.String(), errBuf.String(), exitCode
}

func TestCrictl_Version(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	stdout, stderr, code := runCrictl(t, "version")
	if code != 0 {
		t.Fatalf("crictl version exit=%d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	// crictl version prints a block with RuntimeName, RuntimeVersion, etc.
	if !strings.Contains(stdout, "RuntimeName") && !strings.Contains(stdout, "containerd") {
		t.Errorf("crictl version output missing expected content:\n%s", stdout)
	}
}

func TestCrictl_Info(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	stdout, stderr, code := runCrictl(t, "info")
	if code != 0 {
		t.Fatalf("crictl info exit=%d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	// info prints JSON; the runtime service summary shows at minimum a status map.
	if !strings.Contains(stdout, "RuntimeReady") && !strings.Contains(stdout, "runtimeReady") {
		t.Errorf("crictl info missing runtime readiness field:\n%s", stdout)
	}
}

func TestCrictl_Images(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	// Empty image list is acceptable — we only assert crictl reached the CRI
	// image service and printed the table header.
	stdout, stderr, code := runCrictl(t, "images")
	if code != 0 {
		t.Fatalf("crictl images exit=%d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "IMAGE") && !strings.Contains(stdout, "TAG") {
		t.Errorf("crictl images header missing:\n%s", stdout)
	}
}

func TestCrictl_Ps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	stdout, stderr, code := runCrictl(t, "ps", "-a")
	if code != 0 {
		t.Fatalf("crictl ps exit=%d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "CONTAINER") && !strings.Contains(stdout, "STATE") {
		t.Errorf("crictl ps header missing:\n%s", stdout)
	}
}
