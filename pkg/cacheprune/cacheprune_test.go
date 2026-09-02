package cacheprune

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestResolveTargets(t *testing.T) {
	tests := []struct {
		name        string
		requested   []string
		wantAccept  []string
		wantUnknown []string
	}{
		{
			name:       "empty request means every target",
			requested:  nil,
			wantAccept: []string{TargetBuildKit, TargetContainerd},
		},
		{
			name:       "single target",
			requested:  []string{TargetBuildKit},
			wantAccept: []string{TargetBuildKit},
		},
		{
			// Order is normalized to AllTargets order so the output and
			// any log line are stable regardless of how the caller typed
			// the list.
			name:       "order is normalized, not preserved",
			requested:  []string{TargetContainerd, TargetBuildKit},
			wantAccept: []string{TargetBuildKit, TargetContainerd},
		},
		{
			name:       "duplicates collapse",
			requested:  []string{TargetBuildKit, TargetBuildKit},
			wantAccept: []string{TargetBuildKit},
		},
		{
			name:       "case and whitespace tolerated",
			requested:  []string{"  BuildKit "},
			wantAccept: []string{TargetBuildKit},
		},
		{
			name:        "unknown names are reported, not silently dropped",
			requested:   []string{"images", TargetBuildKit, "nope"},
			wantAccept:  []string{TargetBuildKit},
			wantUnknown: []string{"images", "nope"},
		},
		{
			// Distinct from a nil request: an explicit list of blanks is
			// not "prune everything".
			name:       "blank entries are ignored without becoming prune-all",
			requested:  []string{"", "   "},
			wantAccept: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			accepted, unknown := ResolveTargets(tc.requested)
			if !reflect.DeepEqual(accepted, tc.wantAccept) {
				t.Errorf("accepted = %v, want %v", accepted, tc.wantAccept)
			}
			if !reflect.DeepEqual(unknown, tc.wantUnknown) {
				t.Errorf("unknown = %v, want %v", unknown, tc.wantUnknown)
			}
		})
	}
}

// TestPruner_ImplementsInterface pins the wiring the control server relies
// on: scheduler.Config holds an Interface, and a *Pruner must satisfy it.
func TestPruner_ImplementsInterface(t *testing.T) {
	var _ Interface = (*Pruner)(nil)
}

// TestContainerdPass pins the fix for a documented paper cut: pruneContainerd
// used to take no `all` parameter at all, so `ephemerd cache clear containerd
// --all` was accepted, silently downgraded to the ordinary watermark pass,
// and reported "0 records removed" on a node whose thresholds were not
// tripped. The operator had no override and no hint the flag was ignored.
func TestContainerdPass(t *testing.T) {
	if got := containerdPass(false); got != passWatermark {
		t.Errorf("containerdPass(false) = %q, want %q — a plain prune must stay policy-driven", got, passWatermark)
	}
	if got := containerdPass(true); got != passForced {
		t.Errorf("containerdPass(true) = %q, want %q — --all must reach the collector", got, passForced)
	}
}

// TestPruneContainerd_DisabledIsUnchangedByAll: --all forces the eviction
// POLICY open, it does not conjure a collector. A node without image GC must
// report the same clear reason either way (the requested pass is appended for
// diagnosis; the reason itself does not change).
func TestPruneContainerd_DisabledIsUnchangedByAll(t *testing.T) {
	p := &Pruner{}
	for _, all := range []bool{false, true} {
		res := p.pruneContainerd(context.Background(), all)
		if res.Name != TargetContainerd {
			t.Errorf("all=%v: Name = %q, want %q", all, res.Name, TargetContainerd)
		}
		if res.Err == nil {
			t.Fatalf("all=%v: expected an error when image gc is disabled", all)
		}
		if !strings.Contains(res.Err.Error(), "image gc is disabled") {
			t.Errorf("all=%v: err = %v, want it to name the disabled collector", all, res.Err)
		}
	}
}

// TestPrune_ContainerdTargetThreadsAll walks the PUBLIC entry point to prove
// the flag survives the Prune -> pruneContainerd hop — the hop where it used
// to be dropped silently, so `cache clear containerd --all` ran a watermark
// pass, evicted nothing, and reported success.
//
// It asserts on the pass NAME carried in the disabled-collector error, which
// is the only externally visible evidence of what the flag became. Asserting
// only "one result came back" (what this test used to do) proves nothing: a
// nil ImageGC returns at the disabled guard, so the previous version of this
// test passed identically whether `all` was threaded or hard-coded to false.
func TestPrune_ContainerdTargetThreadsAll(t *testing.T) {
	p := &Pruner{}
	for _, tc := range []struct {
		all  bool
		want collectPass
	}{
		{false, passWatermark},
		{true, passForced},
	} {
		results := p.Prune(context.Background(), []string{TargetContainerd}, tc.all)
		if len(results) != 1 || results[0].Name != TargetContainerd {
			t.Fatalf("all=%v: results = %+v, want one containerd result", tc.all, results)
		}
		if results[0].Err == nil {
			t.Fatalf("all=%v: expected the disabled-collector error", tc.all)
		}
		if got := results[0].Err.Error(); !strings.Contains(got, string(tc.want)) {
			t.Errorf("all=%v: err = %q, want it to name pass %q — the flag did not survive Prune -> pruneContainerd", tc.all, got, tc.want)
		}
	}
}

// TestPrune_ContainerdTargetThreadsAll_DistinguishesThePasses guards the
// assertion above against a degenerate pass naming (e.g. both constants
// becoming the same string), which would make it pass vacuously.
func TestPrune_ContainerdTargetThreadsAll_DistinguishesThePasses(t *testing.T) {
	if passWatermark == passForced {
		t.Fatal("passWatermark and passForced are identical; the threading assertions above cannot distinguish them")
	}
	if strings.Contains(string(passForced), string(passWatermark)) ||
		strings.Contains(string(passWatermark), string(passForced)) {
		t.Fatalf("pass names %q and %q are substrings of one another; a Contains-based assertion would be ambiguous", passWatermark, passForced)
	}
}
