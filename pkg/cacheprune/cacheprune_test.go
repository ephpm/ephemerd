package cacheprune

import (
	"reflect"
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
