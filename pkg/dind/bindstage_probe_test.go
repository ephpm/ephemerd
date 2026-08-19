//go:build !darwin

package dind

import (
	"os"
	"testing"
)

// requireBindStaging skips the calling test unless this process can actually
// perform the bind mount that bind staging depends on.
//
// WHY A PROBE AND NOT os.Geteuid() == 0. Capabilities, user namespaces, LSM
// policy and read-only mount propagation all decide this independently of the
// uid, and ephemerd's own CI runners are containers where the uid and the
// capability disagree — the runner is an unprivileged uid with the default OCI
// capability set, so it has no CAP_SYS_ADMIN even where it looks privileged.
// This reuses probeMountPrivilege for exactly that reason; see the long note
// on it in pull_e2e_test.go.
//
// WHY THE PRODUCTION PATH STILL HARD-FAILS. Skipping here is a statement about
// the test environment, not a licence for the daemon to degrade. Root with
// CAP_SYS_ADMIN is a hard, universal prerequisite for ephemerd on Linux: the
// shipped systemd unit sets no User= and no capability bounding set (see
// cmd/ephemerd/install_linux.go), `ephemerd doctor` FAILS rather than warns
// when euid != 0 (cmd/ephemerd/doctor_linux.go: "ephemerd requires root for
// container management"), the in-process containerd already needs mount(2)
// for the overlayfs snapshotter, and networking shells out to iptables and
// manages a CNI bridge. There is no rootless mode anywhere in the tree or the
// docs. So a daemon that cannot mount(2) is already broken for reasons that
// have nothing to do with bind staging, and staging must never quietly fall
// back to putting a job-controlled path in the OCI spec — that is issue #125
// reopened, green tests and all.
//
// Setting EPHEMERD_TEST_REQUIRE_MOUNT=1 turns the skip into a failure, for
// anywhere this coverage is meant to be guaranteed (a Linux node, or a root
// WSL session).
func requireBindStaging(t *testing.T) {
	t.Helper()

	err := probeMountPrivilege(t)
	if err == nil {
		return
	}
	if os.Getenv(requireMountEnv) != "" {
		t.Fatalf("%s is set, so bind staging coverage must run, but this "+
			"environment cannot mount(2): %v", requireMountEnv, err)
	}
	t.Skipf("bind staging needs mount(2) (CAP_SYS_ADMIN); this environment "+
		"cannot: %v\n"+
		"        to run it:   sudo go test -tags containers_image_openpgp "+
		"-run 'TestBindStaging|TestBindTranslation' -v ./pkg/dind/\n"+
		"        to forbid the skip (fail instead): set %s=1",
		err, requireMountEnv)
}
