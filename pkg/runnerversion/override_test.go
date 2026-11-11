package runnerversion

import (
	"strings"
	"testing"

	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/runner"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func tree(v string) TreeInfo {
	return TreeInfo{Version: v, Parts: runner.ParseVersionString(v), UID: 1001, GID: 1001}
}

func sel(v string) Selection {
	return Selection{Version: v, Parts: runner.ParseVersionString(v), Dir: "/data/runners/" + v + "-linux", Source: metrics.RunnerSourceEmbedded}
}

func TestDecideImageOverride(t *testing.T) {
	m := validManifest("2.338.2", "2.338.0", zeroSHA)
	cases := []struct {
		name      string
		image     TreeInfo
		avail     Selection
		m         *Manifest
		breaker   bool
		override  bool
		needFetch bool
		reason    string
	}{
		// The 2026-11-10 outage shape: image 2.336.0 below floor, embedded
		// 2.337.0 also below floor -> nothing on the node is good enough.
		{"outage: both below floor", tree("2.336.0"), sel("2.337.0"), m, false, false, true, "no verified tree"},
		// After the break-glass fetch landed 2.338.2 in the cache.
		{"outage: cache lifts", tree("2.336.0"), sel("2.338.2"), m, false, true, false, "below manifest floor"},
		// Image below floor, embedded is at the floor: mount embedded.
		{"embedded at floor", tree("2.336.0"), sel("2.338.0"), m, false, true, false, "below manifest floor"},
		// Healthy image is never masked in steady state, even if the node has
		// something newer.
		{"image healthy", tree("2.338.2"), sel("2.339.0"), m, false, false, false, "not stale"},
		// Breaker open, manifest current, node holds only the manifest's
		// version: the manifest is authoritative, no override on the breaker.
		{"breaker open, manifest current", tree("2.338.2"), sel("2.338.2"), m, true, false, false, "not stale"},
		// Breaker open and the node holds a tree NEWER than the manifest's
		// version (a break-glass fetch): the manifest cannot vouch for what
		// it does not know; the breaker's evidence decides -> override.
		{"breaker open, break-glass tree", tree("2.338.2"), sel("2.339.0"), m, true, true, false, "break-glass"},
		// Same tree, breaker closed: nothing (steady state).
		{"breaker closed, newer tree", tree("2.338.2"), sel("2.339.0"), m, false, false, false, "not stale"},
		// Break-glass tree but the image is already newer than it: no override.
		{"breaker open, image newer than break-glass tree", tree("2.339.1"), sel("2.339.0"), m, true, false, true, "no verified tree"},
		// Image at floor exactly: healthy.
		{"image at floor", tree("2.338.0"), sel("2.338.2"), m, false, false, false, "not stale"},
		// No manifest, breaker open, embedded newer: mount embedded.
		{"no manifest, breaker", tree("2.336.0"), sel("2.337.0"), nil, true, true, false, "breaker open"},
		// No manifest, breaker open, nothing newer: fetch.
		{"no manifest, breaker, equal", tree("2.337.0"), sel("2.337.0"), nil, true, false, true, "no verified tree"},
		// No manifest, breaker closed: never override on a hunch.
		{"no manifest, quiet", tree("2.336.0"), sel("2.337.0"), nil, false, false, false, "not stale"},
		// Probe failed: do nothing.
		{"probe failed", TreeInfo{}, sel("2.338.2"), m, true, false, false, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DecideImageOverride(tc.image, tc.avail, tc.m, tc.breaker)
			if d.Override != tc.override || d.NeedFetch != tc.needFetch || !strings.Contains(d.Reason, tc.reason) {
				t.Fatalf("got %+v", d)
			}
		})
	}
}

func TestImageOverrideEngageClear(t *testing.T) {
	var o *ImageOverride
	if _, ok := o.OfficialRunnerOverride("x"); ok {
		t.Fatal("nil override must be inactive")
	}
	o = NewImageOverride(testLog())
	if _, ok := o.OfficialRunnerOverride("ephpm/ephemerd:runner-ci-linux"); ok {
		t.Fatal("fresh override must be inactive")
	}
	o.Engage("/data/runners/verified/extracted/2.338.2-linux", "2.338.2", "ephpm/ephemerd:runner-ci-linux", "test")
	dir, ok := o.OfficialRunnerOverride("ephpm/ephemerd:runner-ci-linux")
	if !ok || dir != "/data/runners/verified/extracted/2.338.2-linux" {
		t.Fatalf("engaged: %q %v", dir, ok)
	}
	if testutil.ToFloat64(metrics.RunnerImageOverrideActive) != 1 {
		t.Fatal("gauge should be 1")
	}
	if v := testutil.ToFloat64(metrics.RunnerVersionInfo.WithLabelValues("2.338.2", metrics.RunnerSourceImageOverride)); v != 1 {
		t.Fatalf("version_info source=image-override = %v", v)
	}
	active, _, ver, _, _, _ := o.Status()
	if !active || ver != "2.338.2" {
		t.Fatal("status")
	}
	o.Clear("image rebuilt")
	if _, ok := o.OfficialRunnerOverride("x"); ok {
		t.Fatal("cleared override must be inactive")
	}
	if testutil.ToFloat64(metrics.RunnerImageOverrideActive) != 0 {
		t.Fatal("gauge should be 0")
	}
}

func TestPrepareOverrideTreeUnknownOwnerIsNoop(t *testing.T) {
	dir := t.TempDir()
	writeRunnerTree(t, dir, "2.338.2")
	if err := PrepareOverrideTree(dir, TreeInfo{UID: -1, GID: -1}); err != nil {
		t.Fatal(err)
	}
}
