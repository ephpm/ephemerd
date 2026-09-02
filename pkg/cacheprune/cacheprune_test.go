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
// report the same clear reason either way.
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

// TestPrune_ContainerdTargetThreadsAll walks the public entry point to prove
// the flag survives the Prune -> pruneContainerd hop (the hop where it used
// to be dropped).
func TestPrune_ContainerdTargetThreadsAll(t *testing.T) {
	p := &Pruner{}
	for _, all := range []bool{false, true} {
		results := p.Prune(context.Background(), []string{TargetContainerd}, all)
		if len(results) != 1 || results[0].Name != TargetContainerd {
			t.Fatalf("all=%v: results = %+v, want one containerd result", all, results)
		}
	}
}
