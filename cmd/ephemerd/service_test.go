package main

import "testing"

// TestActionPastTense covers the "ephemerd stoped" typo: the past tense of
// these verbs is not verb+"ed".
func TestActionPastTense(t *testing.T) {
	cases := map[string]string{
		"stop":    "stopped",
		"start":   "started",
		"restart": "restarted",
		"reload":  "reloaded",
	}
	for action, want := range cases {
		if got := actionPastTense(action); got != want {
			t.Errorf("actionPastTense(%q) = %q, want %q", action, got, want)
		}
	}
}
