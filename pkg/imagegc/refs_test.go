package imagegc

import (
	"reflect"
	"testing"
)

func TestSnapshotRefLabel(t *testing.T) {
	// The exact key matters: it is the only edge containerd's GC follows
	// from an image record to the layers that back it. A typo here would
	// silently make every chain look absent and evict the whole store.
	if got, want := SnapshotRefLabel("overlayfs"), "containerd.io/gc.ref.snapshot.overlayfs"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := SnapshotRefLabel("windows"), "containerd.io/gc.ref.snapshot.windows"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPlanBrokenChains(t *testing.T) {
	chains := []ImageChain{
		{Namespace: "buildkit", Name: "build.ephemerd.local/job-a/app:dev", SnapshotKey: "sha256:gone"},
		{Namespace: "buildkit", Name: "build.ephemerd.local/job-b/app:dev", SnapshotKey: "sha256:live"},
		// Never unpacked — no snapshot to be missing. Must not be evicted:
		// a pushed-but-never-run image is a perfectly normal record.
		{Namespace: "buildkit", Name: "staged/img:v1", SnapshotKey: ""},
		{Namespace: "ephemerd", Name: "ephpm/ephemerd:runner-ci-linux", SnapshotKey: "sha256:gone"},
	}
	existing := map[string]struct{}{"sha256:live": {}}
	protected := map[string]struct{}{"ephpm/ephemerd:runner-ci-linux": {}}

	broken, brokenProtected := PlanBrokenChains(chains, existing, protected)

	wantBroken := []ImageChain{
		{Namespace: "buildkit", Name: "build.ephemerd.local/job-a/app:dev", SnapshotKey: "sha256:gone"},
	}
	if !reflect.DeepEqual(broken, wantBroken) {
		t.Errorf("broken = %+v, want %+v", broken, wantBroken)
	}

	// The pinned runner image is broken too, but evicting it cannot help —
	// it forces a multi-gigabyte re-pull and any container already running
	// on it holds its own rootfs. Report, do not act.
	if len(brokenProtected) != 1 || brokenProtected[0].Name != "ephpm/ephemerd:runner-ci-linux" {
		t.Errorf("brokenProtected = %+v, want the pinned runner image", brokenProtected)
	}
}

func TestPlanBrokenChainsHealthyStore(t *testing.T) {
	// The overwhelmingly common case: nothing is broken and the pass must
	// be a no-op. A false positive here wipes a healthy node's images.
	chains := []ImageChain{
		{Namespace: "buildkit", Name: "a", SnapshotKey: "sha256:one"},
		{Namespace: "ephemerd", Name: "b", SnapshotKey: "sha256:two"},
		{Namespace: "ephemerd", Name: "c"},
	}
	existing := map[string]struct{}{"sha256:one": {}, "sha256:two": {}}

	broken, brokenProtected := PlanBrokenChains(chains, existing, nil)
	if len(broken) != 0 || len(brokenProtected) != 0 {
		t.Fatalf("healthy store planned evictions: broken=%+v protected=%+v", broken, brokenProtected)
	}
}

func TestPlanBrokenChainsIsDeterministic(t *testing.T) {
	chains := []ImageChain{
		{Namespace: "b", Name: "z", SnapshotKey: "sha256:gone"},
		{Namespace: "a", Name: "y", SnapshotKey: "sha256:gone"},
		{Namespace: "a", Name: "x", SnapshotKey: "sha256:gone"},
	}
	first, _ := PlanBrokenChains(chains, nil, nil)
	second, _ := PlanBrokenChains(chains, nil, nil)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeated passes over the same store produced different plans")
	}
	want := []string{"a/x", "a/y", "b/z"}
	for i, ch := range first {
		if ch.Key() != want[i] {
			t.Errorf("position %d = %q, want %q", i, ch.Key(), want[i])
		}
	}
}

func TestChainsReferencing(t *testing.T) {
	// The auto-heal path knows exactly which snapshot vanished and wants
	// only the records that name it — evicting anything else would be
	// collateral damage on a shared node.
	chains := []ImageChain{
		{Namespace: "buildkit", Name: "app:a", SnapshotKey: "sha256:missing"},
		{Namespace: "buildkit", Name: "app:b", SnapshotKey: "sha256:other"},
		{Namespace: "ephemerd", Name: "app:c", SnapshotKey: "sha256:missing"},
		{Namespace: "ephemerd", Name: "app:d"},
	}

	got := ChainsReferencing(chains, "sha256:missing")
	if len(got) != 2 {
		t.Fatalf("got %d chains, want 2: %+v", len(got), got)
	}
	if got[0].Key() != "buildkit/app:a" || got[1].Key() != "ephemerd/app:c" {
		t.Errorf("got %+v, want the two records naming the missing snapshot, sorted", got)
	}

	// An empty key must never match everything — that would evict the
	// whole store on an unparsed error message.
	if got := ChainsReferencing(chains, ""); got != nil {
		t.Errorf("empty key matched %+v", got)
	}
}

func TestImageChainCandidate(t *testing.T) {
	ch := ImageChain{Namespace: "buildkit", Name: "app:dev", SnapshotKey: "sha256:x"}
	cand := ch.Candidate()
	if cand.Namespace != ch.Namespace || cand.Name != ch.Name {
		t.Errorf("Candidate() = %+v, want it to carry namespace and name", cand)
	}
	if cand.Key() != ch.Key() {
		t.Errorf("Key mismatch: %q vs %q", cand.Key(), ch.Key())
	}
}
