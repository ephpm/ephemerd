//go:build linux

package dind

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// End-to-end proof for issue #125 against a REAL runc.
//
// Everything below the OCI spec is out of ephemerd's hands: what actually
// decides whether the escape is closed is what runc's mount(2) resolves, in
// runc's process, in runc's mount namespace, some time after the spec was
// written. Unit tests on the daemon side cannot see that, and the previous
// attempt at this fix shipped precisely because they could not: it produced a
// spec runc rejects outright (EINVAL on every bind, legitimate ones included)
// and the Go tests were green.
//
// So this test writes a bundle, drives the real pinBindSource + mountStager,
// performs the adversarial swap, runs runc, and asserts on what the container
// printed.
//
// PRIVILEGE / OPT-IN. Needs root (mount(2), and runc itself) and a runc
// binary, so it is gated on EPHEMERD_TEST_RUNC. ephemerd ships one; on a node
// it is at <data-dir>/bin/runc, and in a checkout it is
// pkg/containerd/embed/runc. Run it with:
//
//	sudo EPHEMERD_TEST_RUNC=$PWD/pkg/containerd/embed/runc \
//	  go test ./pkg/dind/ -run TestBindStaging_RealRunc -v
//
// The attacker's target holds a benign "SWAPPED" marker, never a symlink to
// "/". The mechanism is identical and it cannot mount a host root into a
// container by accident.

func requireRunc(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("EPHEMERD_TEST_RUNC")
	if bin == "" {
		t.Skip("set EPHEMERD_TEST_RUNC=<path to runc> to run the end-to-end bind escape test")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("EPHEMERD_TEST_RUNC=%s: %v", bin, err)
	}
	return bin
}

// buildContainerInit compiles a tiny static binary that prints what it can
// read at /src/marker. It is the container's whole userspace: building it
// beats depending on a base image being present on whatever machine this runs
// on.
func buildContainerInit(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(dir, "init.go")
	const prog = `package main

import (
	"fmt"
	"os"
)

func main() {
	b, err := os.ReadFile("/src/marker")
	if err != nil {
		fmt.Printf("CONTAINER-SAW-ERR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("CONTAINER-SAW: %s\n", string(b))
}
`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "init")
	cmd := exec.Command("go", "build", "-o", out, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GO111MODULE=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the container init binary here: %v\n%s", err, b)
	}
	return out
}

// writeBundle assembles an OCI bundle whose only interesting mount is
// source -> /src, with the same options ephemerd's withBindMount emits.
func writeBundle(t *testing.T, runcBin, bundle, initBin, source string) {
	t.Helper()
	rootfs := filepath.Join(bundle, "rootfs")
	for _, d := range []string{"proc", "src"} {
		if err := os.MkdirAll(filepath.Join(rootfs, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(initBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "init"), body, 0o755); err != nil {
		t.Fatal(err)
	}

	spec := exec.Command(runcBin, "spec")
	spec.Dir = bundle
	if b, err := spec.CombinedOutput(); err != nil {
		t.Fatalf("runc spec: %v\n%s", err, b)
	}
	raw, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	proc := cfg["process"].(map[string]any)
	proc["args"] = []any{"/init"}
	proc["terminal"] = false
	cfg["root"] = map[string]any{"path": "rootfs", "readonly": false}
	cfg["mounts"] = append(cfg["mounts"].([]any), map[string]any{
		"destination": "/src",
		"type":        "bind",
		"source":      source,
		"options":     []any{"rbind", "rw"},
	})
	patched, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), patched, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runBundle(t *testing.T, runcBin, bundle, id string) string {
	t.Helper()
	stateRoot := filepath.Join(t.TempDir(), "runc-state")
	cmd := exec.Command(runcBin, "--root", stateRoot, "run", id)
	cmd.Dir = bundle
	out, _ := cmd.CombinedOutput()
	t.Cleanup(func() {
		_ = exec.Command(runcBin, "--root", stateRoot, "delete", "-f", id).Run()
	})
	return strings.TrimSpace(string(out))
}

// TestBindStaging_RealRunc_SwapDoesNotLeak runs both halves in one test so a
// green result is meaningful: the control proves the swap really does redirect
// a path-string source (i.e. the bug is reproduced here), and the staged case
// proves it no longer does.
func TestBindStaging_RealRunc_SwapDoesNotLeak(t *testing.T) {
	runcBin := requireRunc(t)
	requireBindStaging(t)

	work := t.TempDir()
	initBin := buildContainerInit(t, work)

	t.Run("control_path_source_leaks", func(t *testing.T) {
		rootfs := filepath.Join(work, "victim-control")
		if err := os.MkdirAll(rootfs, 0o755); err != nil {
			t.Fatal(err)
		}
		plantVictim(t, rootfs)

		bundle := filepath.Join(work, "bundle-control")
		// The pre-fix spec source: the validated path, as a string.
		writeBundle(t, runcBin, bundle, initBin, filepath.Join(rootfs, "a", "real"))
		swapVictim(t, rootfs)

		out := runBundle(t, runcBin, bundle, "eph-toctou-control")
		if !strings.Contains(out, "CONTAINER-SAW: SWAPPED") {
			t.Fatalf("control did not reproduce the escape, so the staged case below proves nothing.\nrunc output: %s", out)
		}
		t.Logf("control (pre-fix shape): %s", out)
	})

	t.Run("staged_source_does_not_leak", func(t *testing.T) {
		root := filepath.Join(work, "data")
		rootfs := filepath.Join(root, "rootfs")
		if err := os.MkdirAll(rootfs, 0o755); err != nil {
			t.Fatal(err)
		}
		plantVictim(t, rootfs)

		stager := newTestStager(t, root, "job-runc")
		pin, err := pinBindSource(rootfs, "/a/real", true)
		if err != nil {
			t.Fatalf("pinning a contained source: %v", err)
		}
		defer func() { _ = pin.Close() }()
		staged, err := stager.stage(pin)
		if err != nil {
			t.Fatalf("staging: %v", err)
		}

		bundle := filepath.Join(work, "bundle-staged")
		writeBundle(t, runcBin, bundle, initBin, staged)
		swapVictim(t, rootfs)

		out := runBundle(t, runcBin, bundle, "eph-toctou-staged")
		if strings.Contains(out, "CONTAINER-SAW: SWAPPED") {
			t.Fatalf("ESCAPE: the container received the attacker's target through the staged source.\nrunc output: %s", out)
		}
		if !strings.Contains(out, "CONTAINER-SAW: PINNED") {
			t.Fatalf("the container did not see the validated content — the bind failed instead of holding, which is the regression the previous fix shipped.\nrunc output: %s", out)
		}
		t.Logf("staged (fixed shape): %s", out)
	})
}

// TestBindStaging_RealRunc_ProcFdSourceIsRejected records why the spec cannot
// simply carry /proc/<ephemerd-pid>/fd/<n>, which is the shape the earlier
// fix/linux-isolation-hardening branch shipped.
//
// runc does not re-resolve the string — its own error echoes it verbatim — but
// mount(2) refuses it, because a bind source must live in the CALLER's mount
// namespace and runc always has its own. That is unconditional: legitimate
// binds fail identically, which makes the fd handoff a functional regression
// rather than a fix. Keeping the finding as an executable test stops the next
// person from re-deriving it from a fleet outage.
func TestBindStaging_RealRunc_ProcFdSourceIsRejected(t *testing.T) {
	runcBin := requireRunc(t)
	requireBindStaging(t)

	work := t.TempDir()
	initBin := buildContainerInit(t, work)

	rootfs := filepath.Join(work, "victim")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	plantVictim(t, rootfs)

	pin, err := pinBindSource(rootfs, "/a/real", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pin.Close() }()
	bundle := filepath.Join(work, "bundle-procfd")
	procSource := "/proc/" + strconv.Itoa(os.Getpid()) + "/fd/" + strconv.Itoa(pin.fd)
	writeBundle(t, runcBin, bundle, initBin, procSource)

	out := runBundle(t, runcBin, bundle, "eph-toctou-procfd")
	if strings.Contains(out, "CONTAINER-SAW:") {
		t.Fatalf("a /proc/<pid>/fd source unexpectedly mounted — if the kernel now allows this, the staging layer could be simplified.\nrunc output: %s", out)
	}
	if !strings.Contains(out, "invalid argument") {
		t.Logf("note: the fd source failed for a reason other than EINVAL; output: %s", out)
	}
	t.Logf("proc-fd source (rejected as expected): %s", out)
}
