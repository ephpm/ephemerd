//go:build linux && e2e && privileged

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/cni"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/runner"
	"github.com/ephpm/ephemerd/pkg/runnerversion"
	"github.com/ephpm/ephemerd/pkg/runtime"
)

// staleOfficialImage is an official runner image whose baked-in runner is
// known to be older than any runner ephemerd has embedded since the
// 2026-11-10 outage (2.336.0 was the exact version that was refused). The
// tag is immutable upstream, so the test's premise does not drift.
const staleOfficialImage = "ghcr.io/actions/actions-runner:2.336.0"

// TestE2E_RunnerOverride_MountsNewerRunnerOverStaleOfficialImage is the
// on-metal proof of the Linux self-heal mechanism
// (docs/arch/self-healing-runner.md, "The mechanism"): with an engaged
// ImageOverride, a job on an official image whose /home/runner is stale
// runs the HOST's verified (embedded) runner, mounted over /home/runner as
// a per-job overlay — not the image's. It asserts:
//
//  1. baseline (no override): the container reports the IMAGE's runner
//     version from /home/runner/bin/Runner.Listener --version;
//  2. override engaged: the same image reports the EMBEDDED (newer)
//     runner's version, i.e. the overlay is what the process sees;
//  3. the process, running as the image's unprivileged `runner` user, can
//     WRITE at the top of /home/runner (run.sh's first action), which is
//     only true if both the lowerdir and the per-job upperdir carry that
//     user's ownership;
//  4. the write landed in the per-job upperdir on the host, under the
//     configured runners directory — never in the shared tree;
//  5. the probe left no view snapshot behind.
//
// It exercises the real overlay through runtime.Create, not a stub; it
// needs root + containerd (the e2e TestMain gate) and pulls the image.
func TestE2E_RunnerOverride_MountsNewerRunnerOverStaleOfficialImage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// The host's verified tree: the embedded runner, extracted the way the
	// daemon does it. The test binary only embeds a runner when `mage
	// download` ran (mage e2eall does); without one there is nothing to
	// mount, so say so rather than fail.
	rm := runner.New(sharedDataDir, sharedLog)
	if err := rm.Extract(); err != nil {
		t.Skipf("no embedded runner in this test binary (run via mage e2eall): %v", err)
	}
	emb, err := runnerversion.ProbeRunnerTree(rm.Dir())
	if err != nil {
		t.Fatalf("probing extracted embedded runner: %v", err)
	}
	t.Logf("embedded runner: %s at %s", emb.Version, rm.Dir())

	// Networking + runtime, as TestE2E_Runtime_CreateWaitDestroy does.
	netDataDir := filepath.Join(sharedDataDir, "net-override")
	if err := os.MkdirAll(netDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cm := cni.New(sharedDataDir, sharedLog)
	if err := cm.Extract(); err != nil {
		t.Fatalf("extracting CNI plugins: %v", err)
	}
	net, err := networking.New(networking.Config{DataDir: netDataDir, CNIBinDir: cm.Dir(), Log: sharedLog})
	if err != nil {
		t.Fatalf("initializing networking: %v", err)
	}
	defer net.Cleanup()
	if err := net.InstallFirewallRules(); err != nil {
		sharedLog.Warn("failed to install firewall rules", "error", err)
	}
	logDir := filepath.Join(sharedDataDir, "logs-override")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	newRuntime := func(src runtime.RunnerOverrideSource) *runtime.Runtime {
		rt, err := runtime.New(runtime.Config{
			Client:         sharedCtrd.Client(),
			Network:        net,
			LogDir:         logDir,
			DataDir:        sharedDataDir,
			RunnerDir:      rm.Dir(),
			RunnerMount:    rm.ContainerDir(),
			RunnerOverride: src,
			Log:            sharedLog,
		})
		if err != nil {
			t.Fatalf("runtime.New: %v", err)
		}
		return rt
	}

	// Probe the image the way boot-verify does (pulls it on first use).
	plain := newRuntime(nil)
	img, err := plain.ProbeImageRunner(ctx, staleOfficialImage)
	if err != nil {
		t.Fatalf("ProbeImageRunner(%s): %v", staleOfficialImage, err)
	}
	t.Logf("image runner: %s owned by %d:%d", img.Version, img.UID, img.GID)
	if img.Version == "" || img.UID < 0 {
		t.Fatalf("probe did not return a version and owner: %+v", img)
	}
	if runner.CompareVersions(emb.Parts, img.Parts) <= 0 {
		t.Skipf("embedded runner %s is not newer than the image's %s; nothing to prove", emb.Version, img.Version)
	}
	if n := plain.SweepRunnerProbeViews(ctx); n != 0 {
		t.Fatalf("probe leaked %d view snapshot(s)", n)
	}

	// runJob runs one container on the stale image whose process reports
	// the runner version it sees at /home/runner and then tries to write at
	// the top of it, as the image's own user. Returns the container log and
	// the per-job host runner dir (valid until Destroy).
	const marker = "e2e-override-write-probe"
	runJob := func(rt *runtime.Runtime, id string) (out string, jobRunnerDir string) {
		t.Helper()
		env, err := rt.Create(ctx, runtime.CreateConfig{
			ID:    id,
			Image: staleOfficialImage,
			Entrypoint: []string{"/bin/sh", "-c",
				"echo \"uid=$(id -u)\"; " +
					"v=$(/home/runner/bin/Runner.Listener --version) && echo \"runner_version=$v\"; " +
					"touch /home/runner/" + marker + " && echo write=ok || echo write=denied; " +
					"ls -ld /home/runner"},
		})
		if err != nil {
			t.Fatalf("Runtime.Create(%s): %v", id, err)
		}
		jobRunnerDir = env.RunnerDir
		if code, err := rt.Wait(ctx, env); err != nil {
			t.Fatalf("Runtime.Wait(%s): %v", id, err)
		} else if code != 0 {
			t.Logf("%s exited %d", id, code)
		}
		logData, err := os.ReadFile(filepath.Join(logDir, id+".log"))
		if err != nil {
			t.Fatalf("reading %s log: %v", id, err)
		}
		out = string(logData)
		t.Logf("%s output:\n%s", id, out)
		// Assert on the host side BEFORE Destroy removes the per-job tree.
		if jobRunnerDir != "" {
			if _, err := os.Stat(filepath.Join(jobRunnerDir, "upper", marker)); err != nil {
				t.Errorf("%s: write probe not found in the per-job upperdir %s: %v", id, jobRunnerDir, err)
			}
		}
		if err := rt.Destroy(ctx, env); err != nil {
			t.Fatalf("Runtime.Destroy(%s): %v", id, err)
		}
		return out, jobRunnerDir
	}

	stamp := time.Now().UnixNano()

	// 1. Baseline: the image's own runner, no mount at all.
	base, baseDir := runJob(plain, fmt.Sprintf("e2e-ovr-base-%d", stamp))
	if !strings.Contains(base, "runner_version="+img.Version) {
		t.Fatalf("baseline must report the image's runner %s", img.Version)
	}
	if baseDir != "" {
		t.Fatalf("baseline job must not get a runner mount, got per-job dir %q", baseDir)
	}

	// 2-4. Override engaged on the embedded tree, prepared for the image's user.
	if err := runnerversion.PrepareOverrideTree(rm.Dir(), img); err != nil {
		t.Fatalf("PrepareOverrideTree: %v", err)
	}
	ov := runnerversion.NewImageOverride(sharedLog)
	ov.Engage(rm.Dir(), emb.Version, staleOfficialImage, "e2e: image "+img.Version+" below embedded "+emb.Version)
	masked := newRuntime(ov)

	out, jobDir := runJob(masked, fmt.Sprintf("e2e-ovr-mask-%d", stamp))
	if !strings.Contains(out, "runner_version="+emb.Version) {
		t.Fatalf("override engaged: expected the mounted runner %s, got:\n%s", emb.Version, out)
	}
	if strings.Contains(out, "runner_version="+img.Version) {
		t.Fatalf("override engaged: the image's stale runner %s is still what the job sees", img.Version)
	}
	if !strings.Contains(out, fmt.Sprintf("uid=%d", img.UID)) {
		t.Fatalf("job did not run as the image's user %d:\n%s", img.UID, out)
	}
	if !strings.Contains(out, "write=ok") {
		t.Fatalf("the image's user cannot write at the top of the overridden /home/runner (run.sh would fail):\n%s", out)
	}
	if jobDir == "" {
		t.Fatal("override job must have a per-job runner dir")
	}
	if filepath.Dir(jobDir) != filepath.Dir(rm.Dir()) {
		t.Fatalf("per-job tree %s must live under the configured runners dir %s", jobDir, filepath.Dir(rm.Dir()))
	}
	if _, err := os.Stat(filepath.Join(rm.Dir(), marker)); err == nil {
		t.Fatal("the job's write reached the SHARED runner tree; the overlay is not isolating jobs")
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("per-job tree %s should be gone after Destroy (err=%v)", jobDir, err)
	}

	// Clearing the override restores the image's runner for the next job.
	ov.Clear("e2e done")
	after, _ := runJob(masked, fmt.Sprintf("e2e-ovr-clear-%d", stamp))
	if !strings.Contains(after, "runner_version="+img.Version) {
		t.Fatalf("after Clear the image's own runner %s must run again:\n%s", img.Version, after)
	}
}
