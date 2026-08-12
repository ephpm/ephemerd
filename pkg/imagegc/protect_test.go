package imagegc

import (
	"testing"
	"time"
)

func TestProtectPrefixed(t *testing.T) {
	const livePrefix = "build.ephemerd.local/live-job/"
	const deadPrefix = "build.ephemerd.local/dead-job/"

	cands := []Candidate{
		{Namespace: "buildkit", Name: livePrefix + "ephpm:dev", LastAccessed: time.Unix(100, 0)},
		{Namespace: "buildkit", Name: deadPrefix + "ephpm:dev", LastAccessed: time.Unix(100, 0)},
		{Namespace: "ephemerd", Name: "ghcr.io/actions/actions-runner:latest", LastAccessed: time.Unix(100, 0)},
	}

	tests := []struct {
		name      string
		prefixes  []string
		protected map[string]struct{}
		wantAdded int
		// wantEvicted is what a maximally-pressured plan should select
		// after the expansion.
		wantEvicted []string
	}{
		{
			// A live job's BuildKit export is referenced by no container,
			// so nothing else marks it live. Evicting it mid-job breaks
			// that job's subsequent docker push.
			name:        "live job prefix protects its build records",
			prefixes:    []string{livePrefix},
			protected:   map[string]struct{}{},
			wantAdded:   1,
			wantEvicted: []string{deadPrefix + "ephpm:dev", "ghcr.io/actions/actions-runner:latest"},
		},
		{
			name:        "no prefixes is a no-op",
			prefixes:    nil,
			protected:   map[string]struct{}{},
			wantAdded:   0,
			wantEvicted: []string{livePrefix + "ephpm:dev", deadPrefix + "ephpm:dev", "ghcr.io/actions/actions-runner:latest"},
		},
		{
			name:      "empty prefix protects nothing (must not match everything)",
			prefixes:  []string{""},
			protected: map[string]struct{}{},
			wantAdded: 0,
		},
		{
			name:      "already-protected names are not double counted",
			prefixes:  []string{livePrefix},
			protected: map[string]struct{}{livePrefix + "ephpm:dev": {}},
			wantAdded: 0,
		},
		{
			name:      "multiple prefixes",
			prefixes:  []string{livePrefix, deadPrefix},
			protected: map[string]struct{}{},
			wantAdded: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ProtectPrefixed(cands, tc.prefixes, tc.protected)
			if got != tc.wantAdded {
				t.Errorf("ProtectPrefixed added %d, want %d", got, tc.wantAdded)
			}
			if tc.wantEvicted == nil {
				return
			}
			plan := PlanEviction(append([]Candidate(nil), cands...), tc.protected, usage(100, 1), defaults)
			if evicted := names(plan.Evict); !equalStrings(evicted, tc.wantEvicted) {
				t.Errorf("plan evicts %v, want %v", evicted, tc.wantEvicted)
			}
		})
	}
}

// ProtectPrefixed must tolerate a nil protected map rather than panicking —
// callers build the set conditionally.
func TestProtectPrefixed_NilSet(t *testing.T) {
	if got := ProtectPrefixed([]Candidate{{Name: "x"}}, []string{"x"}, nil); got != 0 {
		t.Errorf("ProtectPrefixed with a nil set = %d, want 0", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
