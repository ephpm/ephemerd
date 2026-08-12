package imagegc

import (
	"reflect"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/diskspace"
)

const gib = uint64(1024 * 1024 * 1024)

// usage builds a reading for a disk of totalGiB with freeGiB available.
func usage(totalGiB, freeGiB uint64) diskspace.Usage {
	return diskspace.Usage{Path: "/data", TotalBytes: totalGiB * gib, FreeBytes: freeGiB * gib}
}

// cand builds a candidate last accessed ageDays ago, sized sizeGiB.
func cand(name string, ageDays int, sizeGiB float64) Candidate {
	return Candidate{
		Namespace:    "ephemerd",
		Name:         name,
		LastAccessed: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC).AddDate(0, 0, -ageDays),
		SizeBytes:    int64(sizeGiB * float64(gib)),
	}
}

func names(cs []Candidate) []string {
	if len(cs) == 0 {
		return nil
	}
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func set(refs ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, r := range refs {
		m[r] = struct{}{}
	}
	return m
}

// Defaults matching config: 85% high, 70% low, 20 GiB floor, 40 GiB target.
var defaults = Thresholds{
	HighUsedPercent: 85,
	LowUsedPercent:  70,
	MinFreeBytes:    20 * gib,
	TargetFreeBytes: 40 * gib,
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name        string
		usage       diskspace.Usage
		thresholds  Thresholds
		wantOver    bool
		wantReasons []string
		// wantFreeGiB is BytesToFree expressed in GiB; -1 means "don't care".
		wantFreeGiB float64
	}{
		{
			name:       "comfortably under both arms",
			usage:      usage(1000, 500),
			thresholds: defaults,
			wantOver:   false,
		},
		{
			// 1 TB node at 90% used: percentage arm fires, absolute
			// arm does not (100 GiB free is far above the 20 GiB
			// floor). Target is 70% used => 700 GiB used allowed,
			// currently 900 GiB used, so 200 GiB to free.
			name:        "percent arm alone on a large disk",
			usage:       usage(1000, 100),
			thresholds:  defaults,
			wantOver:    true,
			wantReasons: []string{ReasonUsedPercent},
			wantFreeGiB: 200,
		},
		{
			// 100 GiB node with 15 GiB free = 85% used: BOTH arms
			// fire. Percent wants back to 70% used (30 GiB free =>
			// free 15 GiB); absolute wants 40 GiB free => free
			// 25 GiB. The absolute arm is more conservative and
			// wins. This is exactly the small-disk case a
			// percentage alone under-protects: three concurrent
			// jobs at ~5 GiB each would consume the whole 15 GiB.
			name:        "absolute floor is more conservative than percent on a small disk",
			usage:       usage(100, 15),
			thresholds:  defaults,
			wantOver:    true,
			wantReasons: []string{ReasonUsedPercent, ReasonMinFree},
			wantFreeGiB: 25,
		},
		{
			// 2 TB node with 10 GiB free is only 99.5% used but
			// well under the floor — absolute arm alone.
			name:        "absolute arm alone",
			usage:       usage(2000, 10),
			thresholds:  Thresholds{MinFreeBytes: 20 * gib, TargetFreeBytes: 40 * gib},
			wantOver:    true,
			wantReasons: []string{ReasonMinFree},
			wantFreeGiB: 30,
		},
		{
			name:        "percent arm alone when absolute arm disabled",
			usage:       usage(100, 5),
			thresholds:  Thresholds{HighUsedPercent: 85, LowUsedPercent: 70},
			wantOver:    true,
			wantReasons: []string{ReasonUsedPercent},
			wantFreeGiB: 25,
		},
		{
			// A probe that failed and returned a zero reading must
			// never trigger: guessing under uncertainty deletes a
			// warm cache for nothing.
			name:       "zero-capacity reading never triggers",
			usage:      diskspace.Usage{},
			thresholds: defaults,
			wantOver:   false,
		},
		{
			name:       "all arms disabled never triggers",
			usage:      usage(100, 1),
			thresholds: Thresholds{},
			wantOver:   false,
		},
		{
			// low == high degrades to a single threshold rather
			// than erroring; ask for a token byte so the pass still
			// evicts the coldest image.
			name:        "low watermark clamped to high asks for a token byte",
			usage:       usage(100, 15),
			thresholds:  Thresholds{HighUsedPercent: 85, LowUsedPercent: 95},
			wantOver:    true,
			wantReasons: []string{ReasonUsedPercent},
			wantFreeGiB: 0, // 1 byte, rounds to 0 GiB
		},
		{
			// Target below the floor is nonsense; clamp it up so a
			// triggered pass at least restores the floor.
			name:        "target free below min free is clamped up",
			usage:       usage(500, 5),
			thresholds:  Thresholds{MinFreeBytes: 20 * gib, TargetFreeBytes: 1 * gib},
			wantOver:    true,
			wantReasons: []string{ReasonMinFree},
			wantFreeGiB: 15,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.usage, tc.thresholds)
			if got.Over != tc.wantOver {
				t.Fatalf("Over = %v, want %v", got.Over, tc.wantOver)
			}
			if !tc.wantOver {
				if got.BytesToFree != 0 {
					t.Errorf("BytesToFree = %d on an untriggered reading", got.BytesToFree)
				}
				return
			}
			if !reflect.DeepEqual(got.Reasons, tc.wantReasons) {
				t.Errorf("Reasons = %v, want %v", got.Reasons, tc.wantReasons)
			}
			if got.BytesToFree == 0 {
				t.Error("BytesToFree = 0 on a triggered reading; a pass would evict nothing and re-trigger next tick")
			}
			if tc.wantFreeGiB >= 0 {
				if gotGiB := diskspace.GiB(got.BytesToFree); gotGiB < tc.wantFreeGiB-0.01 || gotGiB > tc.wantFreeGiB+1 {
					t.Errorf("BytesToFree = %.2f GiB, want ~%.2f GiB", gotGiB, tc.wantFreeGiB)
				}
			}
		})
	}
}

func TestPlanEviction(t *testing.T) {
	// Five images, coldest first when sorted: old(30d), mid(10d),
	// warm(2d), hot(0d). "pinned" is 60 days cold — the coldest thing on
	// the node — precisely the record an LRU sweep would grab first.
	all := []Candidate{
		cand("hot", 0, 4),
		cand("old", 30, 4),
		cand("warm", 2, 4),
		cand("pinned", 60, 4),
		cand("mid", 10, 4),
	}

	tests := []struct {
		name       string
		candidates []Candidate
		protected  map[string]struct{}
		usage      diskspace.Usage
		thresholds Thresholds
		wantEvict  []string
		// wantShortfall asserts the failsafe signal.
		wantShortfall bool
		wantProtected int
	}{
		{
			name:       "no pressure evicts nothing",
			candidates: all,
			usage:      usage(1000, 900),
			thresholds: defaults,
			wantEvict:  nil,
		},
		{
			// 100 GiB node, 15 GiB free. Absolute arm wants 25 GiB
			// freed; images are 4 GiB each, so seven would be
			// needed but only five exist.
			name:          "evicts LRU-first and reports the shortfall when it runs out",
			candidates:    all,
			usage:         usage(100, 15),
			thresholds:    defaults,
			wantEvict:     []string{"pinned", "old", "mid", "warm", "hot"},
			wantShortfall: true,
		},
		{
			// Same pressure, but the coldest image is the node's
			// pinned runner image. Evicting it would force an
			// immediate re-pull on the very next job.
			name:          "protected image is never selected, however cold",
			candidates:    all,
			protected:     set("pinned"),
			usage:         usage(100, 15),
			thresholds:    defaults,
			wantEvict:     []string{"old", "mid", "warm", "hot"},
			wantProtected: 1,
			wantShortfall: true,
		},
		{
			// 100 GiB node, 14 GiB free, absolute arm target
			// 40 GiB => 26 GiB to free... use a gentler case: a
			// percent-only threshold on a 100 GiB disk at 86% used
			// wants back to 80% used, i.e. 6 GiB freed. Two 4 GiB
			// images cover that; the third must not be touched.
			name:       "stops at the low watermark instead of emptying the store",
			candidates: all,
			usage:      usage(100, 14),
			thresholds: Thresholds{HighUsedPercent: 85, LowUsedPercent: 80},
			wantEvict:  []string{"pinned", "old"},
		},
		{
			name:          "pressure with no candidates reports the full shortfall",
			candidates:    nil,
			usage:         usage(100, 1),
			thresholds:    defaults,
			wantEvict:     nil,
			wantShortfall: true,
		},
		{
			name:          "every candidate protected leaves nothing to do",
			candidates:    all,
			protected:     set("hot", "old", "warm", "pinned", "mid"),
			usage:         usage(100, 1),
			thresholds:    defaults,
			wantEvict:     nil,
			wantProtected: 5,
			wantShortfall: true,
		},
		{
			// Unknown sizes contribute nothing to the budget, so
			// every candidate stays eligible. The collector's
			// re-measure-after-each-delete loop is what stops the
			// pass early in this case, not the plan.
			name: "unknown sizes keep candidates eligible rather than invisible",
			candidates: []Candidate{
				{Namespace: "ephemerd", Name: "a", LastAccessed: time.Unix(200, 0)},
				{Namespace: "ephemerd", Name: "b", LastAccessed: time.Unix(100, 0)},
			},
			usage:         usage(100, 1),
			thresholds:    defaults,
			wantEvict:     []string{"b", "a"},
			wantShortfall: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Copy: PlanEviction sorts a derived slice, but guard
			// against a future change mutating the input.
			in := append([]Candidate(nil), tc.candidates...)
			got := PlanEviction(in, tc.protected, tc.usage, tc.thresholds)

			if diff := names(got.Evict); !reflect.DeepEqual(diff, tc.wantEvict) {
				t.Errorf("Evict = %v, want %v", diff, tc.wantEvict)
			}
			if got.Protected != tc.wantProtected {
				t.Errorf("Protected = %d, want %d", got.Protected, tc.wantProtected)
			}
			if (got.Shortfall > 0) != tc.wantShortfall {
				t.Errorf("Shortfall = %d, want shortfall=%v", got.Shortfall, tc.wantShortfall)
			}
			for _, c := range got.Evict {
				if _, bad := tc.protected[c.Name]; bad {
					t.Fatalf("protected image %q appeared in the eviction plan", c.Name)
				}
			}
		})
	}
}

// TestPlanEviction_TieBreakIsDeterministic guards the property that repeated
// passes over an unchanged store produce an identical plan — otherwise the
// same pressure event would evict a different arbitrary image each tick.
func TestPlanEviction_TieBreakIsDeterministic(t *testing.T) {
	ts := time.Unix(1000, 0)
	in := []Candidate{
		{Namespace: "ephemerd-dind-cache-github-b", Name: "img", LastAccessed: ts},
		{Namespace: "ephemerd", Name: "img", LastAccessed: ts},
		{Namespace: "ephemerd-dind-cache-github-a", Name: "img", LastAccessed: ts},
	}
	// Byte order: '-' (0x2D) sorts before '/' (0x2F), so the dind cache
	// namespaces precede the bare runtime namespace.
	want := []string{
		"ephemerd-dind-cache-github-a/img",
		"ephemerd-dind-cache-github-b/img",
		"ephemerd/img",
	}
	for i := 0; i < 5; i++ {
		plan := PlanEviction(append([]Candidate(nil), in...), nil, usage(100, 1), defaults)
		var got []string
		for _, c := range plan.Evict {
			got = append(got, c.Key())
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("pass %d: order = %v, want %v", i, got, want)
		}
	}
}

func TestPlanByAge(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -7)

	tests := []struct {
		name       string
		candidates []Candidate
		protected  map[string]struct{}
		want       []string
	}{
		{
			name:       "selects only records older than the cutoff, coldest first",
			candidates: []Candidate{cand("fresh", 1, 1), cand("ancient", 90, 1), cand("stale", 8, 1)},
			want:       []string{"ancient", "stale"},
		},
		{
			name:       "protected records survive the age backstop too",
			candidates: []Candidate{cand("ancient", 90, 1), cand("stale", 8, 1)},
			protected:  set("ancient"),
			want:       []string{"stale"},
		},
		{
			// A record with no usable timestamp is unknown, not
			// ancient. Treating it as ancient would nuke every
			// record on a store whose metadata predates both the
			// label and UpdatedAt.
			name: "zero timestamp is never selected",
			candidates: []Candidate{
				{Namespace: "ephemerd", Name: "notime"},
				cand("stale", 8, 1),
			},
			want: []string{"stale"},
		},
		{
			name:       "nothing old enough",
			candidates: []Candidate{cand("fresh", 1, 1), cand("recent", 6, 1)},
			want:       nil,
		},
		{
			name:       "empty input",
			candidates: nil,
			want:       nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := names(PlanByAge(append([]Candidate(nil), tc.candidates...), tc.protected, cutoff))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("PlanByAge = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestThresholds_Enabled(t *testing.T) {
	tests := []struct {
		name string
		t    Thresholds
		want bool
	}{
		{"zero value disabled", Thresholds{}, false},
		{"percent arm only", Thresholds{HighUsedPercent: 85}, true},
		{"absolute arm only", Thresholds{MinFreeBytes: gib}, true},
		{"both", defaults, true},
		{"low watermark alone is not a trigger", Thresholds{LowUsedPercent: 70}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.t.Enabled(); got != tc.want {
				t.Errorf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPressure_ReasonString(t *testing.T) {
	p := Pressure{Reasons: []string{ReasonUsedPercent, ReasonMinFree}}
	if got, want := p.ReasonString(), "used_percent+min_free"; got != want {
		t.Errorf("ReasonString() = %q, want %q", got, want)
	}
	if got := (Pressure{}).ReasonString(); got != "" {
		t.Errorf("ReasonString() on empty = %q, want empty", got)
	}
}
