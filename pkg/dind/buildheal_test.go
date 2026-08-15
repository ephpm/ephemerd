package dind

import (
	"log/slog"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/ephpm/ephemerd/pkg/buildkit"
)

// TestBuildExportUnpacksSoLayersArePinned locks in the fix for the
// reference-awareness half of #149.
//
// containerd only links an image record to its layers through the
// `containerd.io/gc.ref.snapshot.<snapshotter>` label that BuildKit's exporter
// writes on the UNPACK path. Exporting without unpack leaves the record's
// layers pinned by nothing but BuildKit's own cache leases — so once BuildKit
// has a GC policy (#146), collecting those leases orphans the layers under a
// still-resolvable image record. If this attribute ever goes away, that hole
// reopens silently and only shows up as a broken production node.
func TestBuildExportUnpacksSoLayersArePinned(t *testing.T) {
	req := httptest.NewRequest("POST", "/build?t=alpine:local", nil)
	opt, err := dockerBuildOptsToSolveOpt(req, "/tmp/ctx", "test-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opt.Exports) != 1 {
		t.Fatalf("want 1 export, got %d", len(opt.Exports))
	}
	if got := opt.Exports[0].Attrs["unpack"]; got != "true" {
		t.Fatalf("export unpack = %q, want \"true\" — without it the exported image record has no GC reference to its own layers", got)
	}
}

func TestBuildExportUnpacksEvenWithoutTags(t *testing.T) {
	// An untagged build still produces a record BuildKit's GC could orphan.
	req := httptest.NewRequest("POST", "/build", nil)
	opt, err := dockerBuildOptsToSolveOpt(req, "/tmp/ctx", "test-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := opt.Exports[0].Attrs["unpack"]; got != "true" {
		t.Errorf("export unpack = %q, want \"true\"", got)
	}
}

func TestHealNamespacesExcludesTheRuntimeNamespace(t *testing.T) {
	// A repair evicts image records. The runtime namespace holds the node's
	// runner images; losing one forces an immediate multi-gigabyte re-pull
	// on the very next job, so it is out of scope for a repair triggered by
	// one job's build.
	s := &Server{
		jobNamespace:   "ephemerd-dind-job-1",
		cacheNamespace: "ephemerd-dind-cache-github-ephpm",
		log:            slog.Default(),
	}
	bk := &buildkit.Server{}

	got := s.healNamespaces(bk)

	if slices.Contains(got, sharedNamespace) {
		t.Errorf("heal namespaces %v include the runtime namespace %q", got, sharedNamespace)
	}
	for _, want := range []string{"ephemerd-dind-job-1", "ephemerd-dind-cache-github-ephpm"} {
		if !slices.Contains(got, want) {
			t.Errorf("heal namespaces %v missing %q", got, want)
		}
	}
}

func TestHealNamespacesOmitsEmptyNames(t *testing.T) {
	// An empty namespace name is not "no namespace" to containerd's client
	// — it must never reach a List call.
	s := &Server{log: slog.Default()}
	for _, ns := range s.healNamespaces(&buildkit.Server{}) {
		if ns == "" {
			t.Fatalf("heal namespaces contain an empty name: %q", s.healNamespaces(&buildkit.Server{}))
		}
	}
}
