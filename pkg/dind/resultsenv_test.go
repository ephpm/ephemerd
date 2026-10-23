package dind

import (
	"strings"
	"testing"
)

func TestTranslateResultsEnv(t *testing.T) {
	const gh = "https://results-receiver.actions.githubusercontent.com/"
	const lan = "http://10.88.0.1:8089/"
	for _, tc := range []struct {
		name       string
		env        []string
		resultsURL string
		want       string // value of ACTIONS_RESULTS_URL, "" = must be absent
		untouched  bool
	}{
		{"not a patched runner: untouched", []string{"A=1", resultsURLVar + gh}, lan, gh, true},
		{"patched, LAN cache on", []string{"A=1", resultsRenamedVar + gh}, lan, lan, false},
		{"patched, cache off: GitHub's address restored", []string{resultsRenamedVar + gh}, "", gh, false},
		{"patched, stale URL replaced", []string{resultsURLVar + "http://stale/", resultsRenamedVar + gh}, lan, lan, false},
		{"no results vars at all", []string{"A=1"}, lan, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := translateResultsEnv(append([]string(nil), tc.env...), tc.resultsURL)
			if tc.untouched && strings.Join(got, "\n") != strings.Join(tc.env, "\n") {
				t.Fatalf("env changed: %v -> %v", tc.env, got)
			}
			var vals []string
			for _, kv := range got {
				if v, ok := strings.CutPrefix(kv, resultsURLVar); ok {
					vals = append(vals, v)
				}
			}
			switch {
			case tc.want == "" && len(vals) != 0:
				t.Fatalf("ACTIONS_RESULTS_URL = %v, want absent", vals)
			case tc.want != "" && (len(vals) != 1 || vals[0] != tc.want):
				t.Fatalf("ACTIONS_RESULTS_URL = %v, want exactly [%s]", vals, tc.want)
			}
		})
	}
}
