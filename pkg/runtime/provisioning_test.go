package runtime

import "testing"

// TestProvisioning_InFlightIDsSurviveSweep is the regression guard for the
// 2026-08-14 metal race: the orphan sweep decides "orphan" by the absence of a
// containerd container, but a job's runner-dir copy and workdir exist for the
// whole provisioning window (copyDirForJob → NewContainer), which on a cold
// Windows image pull is minutes long. A sweep firing in that window must NOT
// treat the in-flight job as an orphan, or it deletes a live job's runner dir
// and corrupts it into a self-update loop.
func TestProvisioning_InFlightIDsSurviveSweep(t *testing.T) {
	r := &Runtime{}
	const id = "ephemerd-github-ephpm-live_shannon"

	done := r.beginProvisioning(id)

	// The sweep builds its keep set from live containerd containers (none here,
	// mirroring the window before NewContainer runs) and then unions in-flight
	// provisioning IDs.
	keep := map[string]struct{}{}
	r.addProvisioning(keep)
	if _, ok := keep[id]; !ok {
		t.Fatalf("in-flight provisioning id %q missing from the sweep keep set — its runner dir would be deleted mid-provision", id)
	}

	// After provisioning completes the container exists in containerd and the
	// in-flight guard is released; the ID no longer needs the provisioning set.
	done()
	keep2 := map[string]struct{}{}
	r.addProvisioning(keep2)
	if _, ok := keep2[id]; ok {
		t.Errorf("id %q still marked in-flight after done() — leak", id)
	}
}

// TestProvisioning_ConcurrentJobsIndependent ensures one job finishing
// provisioning does not unguard another still in flight.
func TestProvisioning_ConcurrentJobsIndependent(t *testing.T) {
	r := &Runtime{}
	doneA := r.beginProvisioning("job-a")
	_ = r.beginProvisioning("job-b") // still in flight

	doneA()

	keep := map[string]struct{}{}
	r.addProvisioning(keep)
	if _, ok := keep["job-a"]; ok {
		t.Errorf("job-a should be released")
	}
	if _, ok := keep["job-b"]; !ok {
		t.Errorf("job-b must still be guarded while in flight")
	}
}
