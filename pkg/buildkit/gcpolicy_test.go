package buildkit

import (
	"reflect"
	"testing"
	"time"
)

const gib = int64(1024 * 1024 * 1024)

func TestGCConfig_PruneInfo(t *testing.T) {
	full := GCConfig{
		ReservedBytes:         5 * gib,
		MaxUsedBytes:          25 * gib,
		MinFreeBytes:          20 * gib,
		KeepDuration:          7 * 24 * time.Hour,
		EphemeralKeepDuration: 48 * time.Hour,
		EphemeralMaxUsedBytes: 2 * gib,
	}

	tests := []struct {
		name      string
		cfg       GCConfig
		wantRules int
		// check runs extra assertions on the rendered rules.
		check func(t *testing.T, rules []pruneRule)
	}{
		{
			// The bug this whole file exists for: BuildKit's controller
			// is literally `if len(policy) > 0 { prune }`, so an empty
			// policy means the build cache is never collected. The zero
			// config must be recognizable as that state.
			name:      "zero config produces no rules (BuildKit never collects)",
			cfg:       GCConfig{},
			wantRules: 0,
		},
		{
			name:      "disabled overrides every bound",
			cfg:       GCConfig{Disabled: true, ReservedBytes: gib, MaxUsedBytes: gib, EphemeralMaxUsedBytes: gib},
			wantRules: 0,
		},
		{
			name:      "full config renders ephemeral, age, then the unconditional size bound",
			cfg:       full,
			wantRules: 3,
			check: func(t *testing.T, rules []pruneRule) {
				if !reflect.DeepEqual(rules[0].filter, ephemeralFilters) {
					t.Errorf("rule 0 filter = %v, want the ephemeral type filter", rules[0].filter)
				}
				if rules[0].maxUsed != 2*gib {
					t.Errorf("rule 0 maxUsed = %d, want 2 GiB", rules[0].maxUsed)
				}
				if len(rules[1].filter) != 0 {
					t.Errorf("rule 1 filter = %v, want unfiltered (applies to all records)", rules[1].filter)
				}
				if rules[1].reserved != 5*gib || rules[1].maxUsed != 25*gib || rules[1].minFree != 20*gib {
					t.Errorf("rule 1 bounds = %d/%d/%d, want 5/25/20 GiB", rules[1].reserved, rules[1].maxUsed, rules[1].minFree)
				}
				if rules[1].keep != 7*24*time.Hour {
					t.Errorf("rule 1 keep = %v, want 168h", rules[1].keep)
				}
				// The whole point of the third rule: BuildKit treats
				// KeepDuration as an exemption, so a rule carrying one
				// cannot enforce a ceiling on recent records.
				if rules[2].keep != 0 {
					t.Errorf("rule 2 keep = %v, want 0 — a KeepDuration here exempts every recent record from the ceiling", rules[2].keep)
				}
				if len(rules[2].filter) != 0 {
					t.Errorf("rule 2 filter = %v, want unfiltered", rules[2].filter)
				}
				if rules[2].reserved != 5*gib || rules[2].maxUsed != 25*gib || rules[2].minFree != 20*gib {
					t.Errorf("rule 2 bounds = %d/%d/%d, want 5/25/20 GiB", rules[2].reserved, rules[2].maxUsed, rules[2].minFree)
				}
			},
		},
		{
			name:      "only the overall arm configured",
			cfg:       GCConfig{MaxUsedBytes: 10 * gib},
			wantRules: 2,
			check: func(t *testing.T, rules []pruneRule) {
				if len(rules[0].filter) != 0 {
					t.Errorf("expected the unfiltered rule, got filter %v", rules[0].filter)
				}
				if rules[1].keep != 0 || rules[1].maxUsed != 10*gib {
					t.Errorf("rule 1 = keep %v maxUsed %d, want the unconditional 10 GiB bound", rules[1].keep, rules[1].maxUsed)
				}
			},
		},
		{
			// An age-only policy has nothing to bound, so it must NOT
			// grow a size rule with all-zero bounds (which BuildKit reads
			// as "no cap" and would make the rule a no-op anyway).
			name:      "keep duration alone renders one rule, no size bound",
			cfg:       GCConfig{KeepDuration: time.Hour},
			wantRules: 1,
			check: func(t *testing.T, rules []pruneRule) {
				if rules[0].keep != time.Hour {
					t.Errorf("rule 0 keep = %v, want 1h", rules[0].keep)
				}
			},
		},
		{
			name:      "only the ephemeral arm configured",
			cfg:       GCConfig{EphemeralKeepDuration: time.Hour},
			wantRules: 1,
			check: func(t *testing.T, rules []pruneRule) {
				if len(rules[0].filter) == 0 {
					t.Error("expected the ephemeral filter rule")
				}
			},
		},
		{
			// A free-space guard alone is a legitimate policy: collect
			// only when the node is actually tight.
			name:      "min free alone is a valid policy",
			cfg:       GCConfig{MinFreeBytes: 20 * gib},
			wantRules: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.PruneInfo()
			if len(got) != tc.wantRules {
				t.Fatalf("PruneInfo() produced %d rules, want %d", len(got), tc.wantRules)
			}
			if enabled := tc.cfg.Enabled(); enabled != (tc.wantRules > 0) {
				t.Errorf("Enabled() = %v, want %v", enabled, tc.wantRules > 0)
			}
			if tc.check != nil {
				rules := make([]pruneRule, len(got))
				for i, r := range got {
					rules[i] = pruneRule{
						filter:   r.Filter,
						keep:     r.KeepDuration,
						reserved: r.ReservedSpace,
						maxUsed:  r.MaxUsedSpace,
						minFree:  r.MinFreeSpace,
					}
				}
				tc.check(t, rules)
			}
		})
	}
}

// pruneRule mirrors the fields of bkclient.PruneInfo the assertions care
// about, so the test reads without repeating the long field names.
type pruneRule struct {
	filter   []string
	keep     time.Duration
	reserved int64
	maxUsed  int64
	minFree  int64
}
